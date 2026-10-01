package gpumetricsexporter

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// getComputeModeString converts compute mode to human-readable string
func getComputeModeString(mode nvml.ComputeMode) string {
	switch mode {
	case nvml.COMPUTEMODE_DEFAULT:
		return "Default"
	case nvml.COMPUTEMODE_EXCLUSIVE_THREAD:
		return "Exclusive Thread"
	case nvml.COMPUTEMODE_PROHIBITED:
		return "Prohibited"
	case nvml.COMPUTEMODE_EXCLUSIVE_PROCESS:
		return "Exclusive Process"
	default:
		return fmt.Sprintf("Unknown (%d)", mode)
	}
}

const (
	cloudInitInstanceIDFilePath = "/var/lib/cloud/data/instance-id"
)

func (e *GpuMetricsExporter) readInstanceID() (string, error) {
	filePath := e.config.InstanceIDPath
	if filePath == "" {
		filePath = cloudInitInstanceIDFilePath
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	instanceID := strings.TrimSpace(string(data))
	return instanceID, nil
}

// getNVML returns the active NVML interface
func (e *GpuMetricsExporter) getNVML() nvml.Interface {
	return e.nvmlClient
}

// initNVML initializes the NVML library.
//
// If an explicit library path was configured (NvmlLibPath), it uses that path.
// Otherwise, it first attempts to load NVML using the default system dynamic
// linker (dlopen("libnvidia-ml.so.1")), which preserves standard host and VM behavior.
// If the default loader fails to find the library (e.g. running inside a container
// where the driver is located in a GPU Operator container or host mount), it falls back
// to probing well-known candidate paths via FindNVMLLibraryPath.
//
// If initialization returns ERROR_LIB_RM_VERSION_MISMATCH (e.g. after a driver upgrade,
// where glibc marks the previously-loaded library as NODELETE), the exporter exits
// with a non-zero code so the supervisor (systemd or kubelet) can restart it with
// a fresh address space.
func (e *GpuMetricsExporter) initNVML() error {
	if e.config.NvmlLibPath != "" {
		resolvedLibPath := FindNVMLLibraryPath(e.config.NvmlLibPath, e.config.HostRoot)
		e.logInitPath(resolvedLibPath)
		client := nvml.New(nvml.WithLibraryPath(resolvedLibPath))
		ret := client.Init()
		if ret == nvml.ERROR_LIB_RM_VERSION_MISMATCH {
			e.onVersionMismatch(ret)
		}
		if ret != nvml.SUCCESS {
			return fmt.Errorf("nvml init error %s", nvml.ErrorString(ret))
		}
		e.nvmlClient = client
		return nil
	}

	// 1. Try standard dynamic linker first (default behavior, dlopen("libnvidia-ml.so.1"))
	defaultClient := nvml.New()
	ret := defaultClient.Init()
	if ret == nvml.SUCCESS {
		e.logInitPath("")
		e.nvmlClient = defaultClient
		return nil
	}
	if ret == nvml.ERROR_LIB_RM_VERSION_MISMATCH {
		e.onVersionMismatch(ret)
		return fmt.Errorf("nvml init error %s", nvml.ErrorString(ret))
	}

	// 2. If default system dlopen failed (e.g. running in a container where libnvidia-ml
	// is mounted under /run/nvidia/driver or /host), search fallback candidate paths.
	resolvedLibPath := FindNVMLLibraryPath("", e.config.HostRoot)
	if resolvedLibPath != "" {
		fallbackClient := nvml.New(nvml.WithLibraryPath(resolvedLibPath))
		fallbackRet := fallbackClient.Init()
		if fallbackRet == nvml.ERROR_LIB_RM_VERSION_MISMATCH {
			e.onVersionMismatch(fallbackRet)
			return fmt.Errorf("nvml fallback init error (%s): %s", resolvedLibPath, nvml.ErrorString(fallbackRet))
		}
		if fallbackRet == nvml.SUCCESS {
			e.logInitPath(resolvedLibPath)
			e.nvmlClient = fallbackClient
			return nil
		}
		return fmt.Errorf("nvml fallback init error (%s): %s (default init error: %s)",
			resolvedLibPath, nvml.ErrorString(fallbackRet), nvml.ErrorString(ret))
	}

	return fmt.Errorf("nvml init error %s", nvml.ErrorString(ret))
}

func (e *GpuMetricsExporter) logInitPath(path string) {
	if path != e.lastNvmlLibPath || !e.nvmlPathLogged {
		e.nvmlPathLogged = true
		e.lastNvmlLibPath = path
		if path != "" {
			e.log.Info("initializing NVML with library path", zap.String("path", path))
		} else {
			e.log.Info("initializing NVML with default system library")
		}
	} else {
		if path != "" {
			e.log.Debug("initializing NVML with library path", zap.String("path", path))
		} else {
			e.log.Debug("initializing NVML with default system library")
		}
	}
}

// initNVMLForTick initializes NVML for one collection tick. NVML is
// initialized and shut down around every tick rather than once for the
// process lifetime, for two reasons:
//
//   - Open handles to /dev/nvidia* and driver resources are released between ticks,
//     preventing the exporter from blocking driver module unloads or MIG repartitioning.
//   - A failed init is retried on the next tick instead of leaving the
//     exporter shipping empty metrics until the process is restarted.
//
// Note: glibc marks libnvidia-ml.so as NODELETE due to symbol references from cgo,
// so dlclose() does not unmap the library from the process address space. If a driver
// upgrade replaces the kernel driver and causes ERROR_LIB_RM_VERSION_MISMATCH, the
// exporter terminates with a non-zero exit code so its supervisor (systemd or kubelet)
// can restart it with a fresh address space.
//
// Idempotent: a call with NVML already initialized is a no-op. A second Init
// would bump NVML's refcount, and the single paired Shutdown would then
// leave the library open past this tick — silently losing the dlclose-per-
// tick property this whole scheme exists for.
func (e *GpuMetricsExporter) initNVMLForTick() {
	if e.nvmlInitialized {
		e.log.Warn("initNVMLForTick called with NVML already initialized")
		return
	}
	err := e.initNVMLFn()
	if err != nil {
		e.initNVMLError = fmt.Sprintf("e.initNVML error: %v", err)
		e.log.Error("e.initNVML", zap.Error(err))
		e.nvmlInitialized = false
		return
	}
	if e.initNVMLError != "" {
		e.log.Info("nvml init recovered after previous failure",
			zap.String("previous_error", e.initNVMLError))
	}
	e.initNVMLError = ""
	e.nvmlInitialized = true
}

// shutdownNVML shuts down the NVML library, releasing the dlopen handle so a
// library update on disk is not held hostage by this process.
//
// Idempotent, and safe after a failed initNVMLForTick: go-nvml's Shutdown
// calls into the library before checking its own refcount, so a shutdown
// with no library open is an unresolved cgo symbol — a SIGABRT that no
// recover() can contain. The nvmlInitialized guard is what makes an
// unconditional `defer e.shutdownNVML()` in queryMetrics safe: on a tick
// where init failed it no-ops instead of aborting the process. That is the
// normal failed-tick path, hence Debug, not a warning.
func (e *GpuMetricsExporter) shutdownNVML() {
	if !e.nvmlInitialized {
		e.log.Debug("shutdownNVML skipped, NVML not initialized this tick")
		return
	}
	e.nvmlInitialized = false
	e.shutdownNVMLFn()
}

// shutdownNVMLReal is the production NVML shutdown behind shutdownNVMLFn.
//
// WARNING: go-nvml v0.13 mutates a package-global on load/close —
// errorStringFunc is swapped in library.load() and reset in library.close()
// with no synchronization. That is safe here only because every nvml.*
// package-level call in this exporter happens on the query goroutine, the
// same goroutine this runs on. Never call nvml package functions from
// another goroutine (e.g. the send loop or a future consumer) while this
// per-tick init/shutdown cycle is active: it would be a data race on that
// global, and the race detector cannot see it across the cgo boundary.
func (e *GpuMetricsExporter) shutdownNVMLReal() {
	if e.nvmlClient != nil {
		ret := e.nvmlClient.Shutdown()
		if ret != nvml.SUCCESS {
			err := fmt.Errorf("nvml shutdown error %s", nvml.ErrorString(ret))
			e.log.Error("nvml.Shutdown", zap.Error(err))
		}
		e.nvmlClient = nil
	}
}

// getDeviceCount returns the number of NVIDIA GPUs
func (e *GpuMetricsExporter) getDeviceCount() (int, error) {
	count, ret := e.getNVML().DeviceGetCount()
	if ret != nvml.SUCCESS {
		err := fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
		e.log.Error("nvml.DeviceGetCount", zap.Error(err))
		return 0, err
	}
	return count, nil
}

// getDeviceHandle returns the device handle for a given GPU index
func (e *GpuMetricsExporter) getDeviceHandle(index int) (nvml.Device, error) {
	device, ret := e.getNVML().DeviceGetHandleByIndex(index)
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to get device at index %d: %v", index, nvml.ErrorString(ret))
	}
	return device, nil
}

// queryMetrics builds one GpuMetrics payload for the current tick: reads the
// instance ID, stamps exporter health metadata, and for each GPU runs
// collectGPUInfo. It also collects fabric-manager status and recent XID/SXID
// errors. Unsupported NVML features silently no-op rather than failing the
// whole tick.
func (e *GpuMetricsExporter) queryMetrics() (*gpumetrics.GpuMetrics, error) {
	// Initialize NVML for this tick and shut it down after collection (see
	// initNVMLForTick/shutdownNVML): retries a failed init on each tick and
	// releases driver device handles between ticks. Both are guarded on
	// nvmlInitialized, so the unconditional shutdown is a no-op on a tick
	// whose init failed.
	e.initNVMLForTick()
	defer e.shutdownNVML()

	metrics := gpumetrics.NewGpuMetrics()

	instanceID, err := e.readInstanceID()
	if err != nil {
		e.log.Error("e.readInstanceID", zap.Error(err))
		metrics.ExporterInfo.ReadInstanceIDError = fmt.Sprintf("e.readInstanceID error: %v", err)
	} else {
		metrics.Source.InstanceID = instanceID
	}

	metrics.ExporterInfo.Seqno = e.seqno.Add(1)
	metrics.ExporterInfo.Timestamp = time.Now().UTC().Unix()
	metrics.ExporterInfo.Version = e.config.Version
	metrics.ExporterInfo.StartTime = e.startTime
	metrics.ExporterInfo.InitNVMLError = e.initNVMLError
	// These two are written by the send goroutine under lastMetricsMu; read
	// them under the same lock to avoid a torn int64 / string race.
	e.lastMetricsMu.RLock()
	metrics.ExporterInfo.SendMetricsErrorCount = e.sendMetricsErrorCount
	metrics.ExporterInfo.SendMetricsLastError = e.sendMetricsLastError
	e.lastMetricsMu.RUnlock()

	if e.initNVMLError == "" {
		count, err := e.getDeviceCount()
		if err != nil {
			e.log.Error("e.getDeviceCount", zap.Error(err))
			metrics.ExporterInfo.GetDeviceCountError = fmt.Sprintf("e.getDeviceCount error: %v", err)
		} else {
			metrics.GpuDeviceCount = count
			for i := 0; i < count; i++ {
				gpuInfo := e.collectGPUInfo(i)
				metrics.Gpus = append(metrics.Gpus, gpuInfo)
			}
		}
	}

	// After collection, with NVML still loaded: what was mapped while the
	// numbers above were read.
	metrics.NVMLLibrary = e.inspectNVMLLibrary()

	status, err := e.checkFabricManager()
	if err != nil {
		metrics.NvFabricManagerStatus.Error = fmt.Sprintf("e.checkFabricManager error: %v", err)
	} else {
		metrics.NvFabricManagerStatus = status
	}

	xidErrors, err := e.getXIDErrors()
	if err != nil {
		metrics.XIDErrors.Error = fmt.Sprintf("e.getXIDErrors error: %v", err)
	} else {
		metrics.XIDErrors.XIDErrors = xidErrors
	}
	// Cumulative, and reported even on a tick where dmesg itself failed: a
	// non-zero count means XID lines were lost and must not go unnoticed.
	metrics.XIDErrors.DroppedCount = e.xidErrorsDroppedCount()

	// Publish this snapshot as the delta baseline for the next tick. Computing
	// deltas against the last *collected* snapshot (not the last *sent* one)
	// keeps *_Delta fields additive even when several payloads backlog in
	// metricsQueue while a send is stalled: each tick diffs against the
	// immediately-previous snapshot, so summing the deltas of backlogged
	// payloads still reconstructs the true cumulative increase.
	e.lastMetricsMu.Lock()
	e.lastMetrics = metrics
	e.lastMetricsMu.Unlock()

	return metrics, nil
}

// collectGPUInfo collects all information for a single GPU
func (e *GpuMetricsExporter) collectGPUInfo(index int) gpumetrics.GPUInfo {
	gpuInfo := gpumetrics.GPUInfo{Index: index}

	device, err := e.getDeviceHandle(index)
	if err != nil {
		gpuInfo.Error = fmt.Sprintf("e.getDeviceHandle error: %v", err)
		return gpuInfo
	}

	e.collectBasicInfo(device, &gpuInfo)
	e.collectTemperature(device, &gpuInfo)
	e.collectPowerInfo(device, &gpuInfo)
	e.collectMemoryInfo(device, &gpuInfo)
	e.collectBAR1MemoryInfo(device, &gpuInfo)
	e.collectUtilization(device, &gpuInfo)
	e.collectDecoderEncoderUtil(device, &gpuInfo)
	e.collectClockSpeeds(device, &gpuInfo)
	e.collectFanSpeed(device, &gpuInfo)
	e.collectPCIInfo(device, &gpuInfo)
	e.collectDriverInfo(&gpuInfo)
	e.collectVBiosInfo(device, &gpuInfo)
	e.collectArchitecture(device, &gpuInfo)
	e.collectCUDACores(device, &gpuInfo)
	e.collectComputeMode(device, &gpuInfo)
	e.collectPerformanceState(device, &gpuInfo)
	e.collectPersistenceMode(device, &gpuInfo)
	e.collectClocksThrottleInfo(device, &gpuInfo)
	e.collectECCInfo(device, gpuInfo.UUID, &gpuInfo)
	e.collectRowRemappingInfo(device, gpuInfo.UUID, &gpuInfo)
	e.collectNvLinkInfo(device, gpuInfo.UUID, &gpuInfo)
	e.collectMIGInfo(device, &gpuInfo)

	return gpuInfo
}

// collectBasicInfo collects basic GPU information (UUID, name)
func (e *GpuMetricsExporter) collectBasicInfo(device nvml.Device, info *gpumetrics.GPUInfo) {
	if uuid, ret := device.GetUUID(); ret == nvml.SUCCESS {
		info.UUID = uuid
	}

	if name, ret := device.GetName(); ret == nvml.SUCCESS {
		info.Name = name
	}

	if serial, ret := device.GetSerial(); ret == nvml.SUCCESS {
		info.Serial = serial
	}
}

// collectTemperature collects GPU temperature AND memory temperature
func (e *GpuMetricsExporter) collectTemperature(device nvml.Device, info *gpumetrics.GPUInfo) {
	if temp, ret := device.GetTemperature(nvml.TEMPERATURE_GPU); ret == nvml.SUCCESS {
		info.Temperature = uint(temp)
	}

	// Get memory temperature using field values. GetFieldValues returns SUCCESS
	// for the batch even when an individual field is unsupported, so each
	// field's own NvmlReturn must be checked before reading its Value (an
	// unsupported field leaves the [8]byte Value zeroed/garbage).
	fieldValues := []nvml.FieldValue{
		{FieldId: nvml.FI_DEV_MEMORY_TEMP},
	}

	if ret := device.GetFieldValues(fieldValues); ret == nvml.SUCCESS {
		if fv := &fieldValues[0]; nvml.Return(fv.NvmlReturn) == nvml.SUCCESS {
			info.MemoryTemp = uint(binary.LittleEndian.Uint32(fv.Value[:]))
		}
	}
}

// collectPowerInfo collects GPU power usage and limits
func (e *GpuMetricsExporter) collectPowerInfo(device nvml.Device, info *gpumetrics.GPUInfo) {
	if power, ret := device.GetPowerUsage(); ret == nvml.SUCCESS {
		info.PowerUsage = uint(power / 1000) // Convert mW to W
	}

	if limit, ret := device.GetPowerManagementLimit(); ret == nvml.SUCCESS {
		info.PowerLimit = uint(limit / 1000) // Convert mW to W
	}
}

// collectMemoryInfo collects GPU memory information
func (e *GpuMetricsExporter) collectMemoryInfo(device nvml.Device, info *gpumetrics.GPUInfo) {
	if memInfo, ret := device.GetMemoryInfo(); ret == nvml.SUCCESS {
		info.MemoryTotal = memInfo.Total
		info.MemoryUsed = memInfo.Used
		info.MemoryFree = memInfo.Free
	}
}

// collectBAR1MemoryInfo collects BAR1 memory information
func (e *GpuMetricsExporter) collectBAR1MemoryInfo(device nvml.Device, info *gpumetrics.GPUInfo) {
	if bar1MemInfo, ret := device.GetBAR1MemoryInfo(); ret == nvml.SUCCESS {
		info.BAR1MemoryTotal = bar1MemInfo.Bar1Total
		info.BAR1MemoryUsed = bar1MemInfo.Bar1Used
		info.BAR1MemoryFree = bar1MemInfo.Bar1Free
	}
}

// collectUtilization collects GPU utilization rates
func (e *GpuMetricsExporter) collectUtilization(device nvml.Device, info *gpumetrics.GPUInfo) {
	if utilization, ret := device.GetUtilizationRates(); ret == nvml.SUCCESS {
		info.Utilization = gpumetrics.GPUUtilization{
			GPU:    uint(utilization.Gpu),
			Memory: uint(utilization.Memory),
		}
	}
}

// collectDecoderEncoderUtil collects decoder and encoder utilization
func (e *GpuMetricsExporter) collectDecoderEncoderUtil(device nvml.Device, info *gpumetrics.GPUInfo) {
	if decoderUtil, _, ret := device.GetDecoderUtilization(); ret == nvml.SUCCESS {
		info.DecoderUtil = uint(decoderUtil)
	}

	if encoderUtil, _, ret := device.GetEncoderUtilization(); ret == nvml.SUCCESS {
		info.EncoderUtil = uint(encoderUtil)
	}
}

// collectClockSpeeds collects GPU clock speeds
func (e *GpuMetricsExporter) collectClockSpeeds(device nvml.Device, info *gpumetrics.GPUInfo) {
	if graphicsClock, ret := device.GetClockInfo(nvml.CLOCK_GRAPHICS); ret == nvml.SUCCESS {
		info.Clocks.Graphics = uint(graphicsClock)
	}

	if memoryClock, ret := device.GetClockInfo(nvml.CLOCK_MEM); ret == nvml.SUCCESS {
		info.Clocks.Memory = uint(memoryClock)
	}

	if smClock, ret := device.GetClockInfo(nvml.CLOCK_SM); ret == nvml.SUCCESS {
		info.Clocks.SM = uint(smClock)
	}
}

func (e *GpuMetricsExporter) collectFanSpeed(device nvml.Device, info *gpumetrics.GPUInfo) {
	fanSpeed, ret := device.GetFanSpeed()
	switch ret {
	case nvml.SUCCESS:
		info.FanSpeed = uint(fanSpeed)
	case nvml.ERROR_NOT_SUPPORTED:
		// Device doesn't have a fan - this is normal for data center GPUs
		info.FanSpeed = 0
	}
}

func (e *GpuMetricsExporter) collectPCIInfo(device nvml.Device, info *gpumetrics.GPUInfo) {
	pciInfo, ret := device.GetPciInfo()
	if ret != nvml.SUCCESS {
		return
	}

	// Fix: Use the correct BusID format with domain included
	info.PCI = gpumetrics.PCIInfo{
		BusID:  fmt.Sprintf("%04x:%02x:%02x.%x", pciInfo.Domain, pciInfo.Bus, pciInfo.Device, 0),
		Domain: uint(pciInfo.Domain),
		Bus:    uint(pciInfo.Bus),
		Device: uint(pciInfo.Device),
	}

	// Get current PCIe link info
	if gen, ret := device.GetCurrPcieLinkGeneration(); ret == nvml.SUCCESS {
		info.PCI.PCIGen = uint(gen)
	}

	if width, ret := device.GetCurrPcieLinkWidth(); ret == nvml.SUCCESS {
		info.PCI.LinkWidth = uint(width)
	}

	// Get max PCIe capabilities
	if maxGen, ret := device.GetGpuMaxPcieLinkGeneration(); ret == nvml.SUCCESS {
		info.PCI.MaxPCIGen = uint(maxGen)
	}

	if maxWidth, ret := device.GetMaxPcieLinkWidth(); ret == nvml.SUCCESS {
		info.PCI.MaxLinkWidth = uint(maxWidth)
	}

	// If you also want to monitor actual throughput (which can be 0 when idle),
	// consider adding a separate field for it
	if rxThroughput, ret := device.GetPcieThroughput(nvml.PCIE_UTIL_RX_BYTES); ret == nvml.SUCCESS {
		// This is actual data transfer rate, not link speed
		// Only useful when GPU is active
		e.log.Debug("PCIe RX throughput", zap.Uint("kbytes_per_sec", uint(rxThroughput)))
		info.PCI.RxThroughput = rxThroughput
	}

	if txThroughput, ret := device.GetPcieThroughput(nvml.PCIE_UTIL_TX_BYTES); ret == nvml.SUCCESS {
		e.log.Debug("PCIe TX throughput", zap.Uint("kbytes_per_sec", uint(txThroughput)))
		info.PCI.TxThroughput = txThroughput
	}
}

// collectDriverInfo collects GPU driver information
func (e *GpuMetricsExporter) collectDriverInfo(info *gpumetrics.GPUInfo) {
	if driverVersion, ret := e.getNVML().SystemGetDriverVersion(); ret == nvml.SUCCESS {
		info.DriverVersion = driverVersion
	}
}

// collectVBiosInfo collects GPU VBIOS version
func (e *GpuMetricsExporter) collectVBiosInfo(device nvml.Device, info *gpumetrics.GPUInfo) {
	if vbios, ret := device.GetVbiosVersion(); ret == nvml.SUCCESS {
		info.VBios = vbios
	}
}

// collectComputeMode collects GPU compute mode
func (e *GpuMetricsExporter) collectComputeMode(device nvml.Device, info *gpumetrics.GPUInfo) {
	if computeMode, ret := device.GetComputeMode(); ret == nvml.SUCCESS {
		info.ComputeModeValue = int(computeMode)
		info.ComputeMode = getComputeModeString(computeMode)
	}
}

// collectPerformanceState collects GPU performance state
func (e *GpuMetricsExporter) collectPerformanceState(device nvml.Device, info *gpumetrics.GPUInfo) {
	if perfState, ret := device.GetPerformanceState(); ret == nvml.SUCCESS {
		info.PerformanceStateValue = int(perfState)
		info.PerformanceState = fmt.Sprintf("P%d", perfState)
	}
}

func (e *GpuMetricsExporter) collectPersistenceMode(device nvml.Device, info *gpumetrics.GPUInfo) {
	if persistenceMode, ret := device.GetPersistenceMode(); ret == nvml.SUCCESS {
		info.PersistenceMode = int(persistenceMode)
	}
}

// collectArchitecture collects GPU architecture information
func (e *GpuMetricsExporter) collectArchitecture(device nvml.Device, info *gpumetrics.GPUInfo) {
	if arch, ret := device.GetArchitecture(); ret == nvml.SUCCESS {
		switch arch {
		case nvml.DEVICE_ARCH_KEPLER:
			info.Architecture = "Kepler"
		case nvml.DEVICE_ARCH_MAXWELL:
			info.Architecture = "Maxwell"
		case nvml.DEVICE_ARCH_PASCAL:
			info.Architecture = "Pascal"
		case nvml.DEVICE_ARCH_VOLTA:
			info.Architecture = "Volta"
		case nvml.DEVICE_ARCH_TURING:
			info.Architecture = "Turing"
		case nvml.DEVICE_ARCH_AMPERE:
			info.Architecture = "Ampere"
		case nvml.DEVICE_ARCH_ADA:
			info.Architecture = "Ada Lovelace"
		case nvml.DEVICE_ARCH_HOPPER:
			info.Architecture = "Hopper"
		case nvml.DEVICE_ARCH_BLACKWELL:
			info.Architecture = "Blackwell"
		default:
			info.Architecture = "Unknown"
		}
	}
}

// collectCUDACores collects CUDA compute capability
func (e *GpuMetricsExporter) collectCUDACores(device nvml.Device, info *gpumetrics.GPUInfo) {
	major, minor, ret := device.GetCudaComputeCapability()
	if ret == nvml.SUCCESS {
		info.CUDAComputeCapability.Major = major
		info.CUDAComputeCapability.Minor = minor
	}
}

// collectClocksThrottleInfo collects clocks throttle reasons
func (e *GpuMetricsExporter) collectClocksThrottleInfo(device nvml.Device, info *gpumetrics.GPUInfo) {
	if reasons, ret := device.GetCurrentClocksThrottleReasons(); ret == nvml.SUCCESS {
		info.ClocksThrottle.ThrottleReasons = reasons
		info.ClocksThrottle.ThrottleReasonsStr = parseThrottleReasons(reasons)
	}

	if events, ret := device.GetCurrentClocksEventReasons(); ret == nvml.SUCCESS {
		info.ClocksThrottle.EventReasons = events
		info.ClocksThrottle.EventReasonsStr = parseEventReasons(events)
	}
}

// collectECCInfo collects the GPU's ECC mode, error counters and retired pages.
//
// NOT_SUPPORTED from GetEccMode is a card without ECC, not a fault, and leaves
// Supported false with no error. Any other failure is recorded: it used to be a
// bare return, which on the host was indistinguishable from ECC switched off.
func (e *GpuMetricsExporter) collectECCInfo(device nvml.Device, gpuID string, info *gpumetrics.GPUInfo) {
	currentMode, pendingMode, ret := device.GetEccMode()
	switch ret {
	case nvml.SUCCESS:
	case nvml.ERROR_NOT_SUPPORTED:
		return
	default:
		info.ECC.Error = fmt.Sprintf("device.GetEccMode error: %s", nvml.ErrorString(ret))
		return
	}

	info.ECC.Supported = true
	info.ECC.Enabled = currentMode == 1
	info.ECC.Pending = pendingMode == 1

	if currentMode == 1 {
		info.ECC.Mode = "Enabled"
		e.collectECCErrors(device, gpuID, &info.ECC)
		e.collectRetiredPages(device, gpuID, &info.ECC)
	} else {
		info.ECC.Mode = "Disabled"
	}
}

// getValueDelta returns the monotonic increase of a cumulative counter: the
// difference newValue - oldValue when newValue is greater, otherwise 0. A
// decrease (counter reset, reboot, or counter wraparound past the int64 range)
// is treated as a reset and yields 0.
func getValueDelta(newValue uint64, oldValue uint64) int64 {
	if int64(newValue) > int64(oldValue) {
		return int64(newValue) - int64(oldValue)
	} else {
		return 0
	}
}

// collectRetiredPages collects the GPU's retired-page counts.
//
// Ampere and later replaced page retirement with row remapping and report none
// of the FI_DEV_RETIRED_* fields, so Supported false with no error is the
// normal answer across the modern fleet — read RowRemappingInfo there instead.
func (e *GpuMetricsExporter) collectRetiredPages(device nvml.Device, gpuID string, eccInfo *gpumetrics.ECCInfo) {
	fieldValues := []nvml.FieldValue{
		{FieldId: nvml.FI_DEV_RETIRED_SBE},
		{FieldId: nvml.FI_DEV_RETIRED_DBE},
		{FieldId: nvml.FI_DEV_RETIRED_PENDING},
	}

	switch ret := device.GetFieldValues(fieldValues); ret {
	case nvml.SUCCESS:
	case nvml.ERROR_NOT_SUPPORTED:
		return
	default:
		eccInfo.RetiredPages.Error = fmt.Sprintf("GetFieldValues error: %s", nvml.ErrorString(ret))
		return
	}

	// GetFieldValues returns SUCCESS for the batch even when individual
	// fields are unsupported; read each field's Value only when its own
	// NvmlReturn is SUCCESS, otherwise the [8]byte is uninitialised and
	// would feed garbage into the delta logic below.
	got := 0
	if fv := &fieldValues[0]; nvml.Return(fv.NvmlReturn) == nvml.SUCCESS {
		eccInfo.RetiredPages.SBEPages = binary.LittleEndian.Uint64(fv.Value[:])
		got++
	}
	if fv := &fieldValues[1]; nvml.Return(fv.NvmlReturn) == nvml.SUCCESS {
		eccInfo.RetiredPages.DBEPages = binary.LittleEndian.Uint64(fv.Value[:])
		got++
	}
	if fv := &fieldValues[2]; nvml.Return(fv.NvmlReturn) == nvml.SUCCESS {
		eccInfo.RetiredPages.PendingPages = binary.LittleEndian.Uint64(fv.Value[:])
		got++
	}

	if got == 0 {
		return
	}

	eccInfo.RetiredPages.Supported = true

	lastGpuInfo := e.findLastGpuInfo(gpuID)
	if lastGpuInfo != nil {
		eccInfo.RetiredPages.SBEPagesDelta = getValueDelta(eccInfo.RetiredPages.SBEPages, lastGpuInfo.ECC.RetiredPages.SBEPages)
		eccInfo.RetiredPages.DBEPagesDelta = getValueDelta(eccInfo.RetiredPages.DBEPages, lastGpuInfo.ECC.RetiredPages.DBEPages)
		eccInfo.RetiredPages.PendingPagesDelta = getValueDelta(eccInfo.RetiredPages.PendingPages, lastGpuInfo.ECC.RetiredPages.PendingPages)
	}
}

// collectECCErrorsInternal fills the volatile and aggregate ECC counters for
// one memory location.
//
// NOT_SUPPORTED is not a failure here: a MIG-enabled GPU declines the whole
// volatile scope that way while still serving the aggregate one, which is why
// Supported lives on the scope rather than on the GPU.
func (e *GpuMetricsExporter) collectECCErrorsInternal(device nvml.Device, memoryLocation nvml.MemoryLocation, result *gpumetrics.ECCErrorsCounters) {
	queries := []struct {
		scope     *gpumetrics.ECCErrors
		dst       *uint64
		errorType nvml.MemoryErrorType
		counter   nvml.EccCounterType
		what      string
	}{
		{&result.Volatile, &result.Volatile.Correctable, nvml.MEMORY_ERROR_TYPE_CORRECTED, nvml.VOLATILE_ECC, "volatile ECC correctable"},
		{&result.Volatile, &result.Volatile.Uncorrectable, nvml.MEMORY_ERROR_TYPE_UNCORRECTED, nvml.VOLATILE_ECC, "volatile ECC uncorrectable"},
		{&result.Aggregate, &result.Aggregate.Correctable, nvml.MEMORY_ERROR_TYPE_CORRECTED, nvml.AGGREGATE_ECC, "aggregate ECC correctable"},
		{&result.Aggregate, &result.Aggregate.Uncorrectable, nvml.MEMORY_ERROR_TYPE_UNCORRECTED, nvml.AGGREGATE_ECC, "aggregate ECC uncorrectable"},
	}

	for _, q := range queries {
		errors, ret := device.GetMemoryErrorCounter(q.errorType, q.counter, memoryLocation)
		switch ret {
		case nvml.SUCCESS:
			*q.dst = errors
			q.scope.Supported = true
		case nvml.ERROR_NOT_SUPPORTED:
		default:
			e.log.Warn("Failed to get ECC counter",
				zap.String("counter", q.what),
				zap.String("error", nvml.ErrorString(ret)),
				zap.Int32("memoryLocation", int32(memoryLocation)))

			// First failure in the scope wins. Each of the four queries used to
			// overwrite the previous one's message, so only the last was ever
			// visible — the same trap the NVLink counters had.
			if q.scope.Error == "" {
				q.scope.Error = fmt.Sprintf("failed to get %s: %s", q.what, nvml.ErrorString(ret))
			}
		}
	}
}

// findLastGpuInfo returns the previous-tick GPUInfo for the given GPU UUID, or
// nil if none is known yet. It matches GPUs across ticks by UUID so cumulative
// counters can be diffed into per-tick deltas.
func (e *GpuMetricsExporter) findLastGpuInfo(gpuID string) *gpumetrics.GPUInfo {
	e.lastMetricsMu.RLock()
	defer e.lastMetricsMu.RUnlock()

	if e.lastMetrics != nil {
		for i := range e.lastMetrics.Gpus {
			gpuInfo := &e.lastMetrics.Gpus[i]
			if gpuInfo.UUID == gpuID {
				return gpuInfo
			}
		}
	}

	return nil
}

// collectECCErrors collects ECC error counts (both volatile and aggregate)
func (e *GpuMetricsExporter) collectECCErrors(device nvml.Device, gpuID string, eccInfo *gpumetrics.ECCInfo) {
	e.collectECCErrorsInternal(device, nvml.MEMORY_LOCATION_DEVICE_MEMORY, &eccInfo.DRAMErrors)
	e.collectECCErrorsInternal(device, nvml.MEMORY_LOCATION_SRAM, &eccInfo.SRAMErrors)

	lastGpuInfo := e.findLastGpuInfo(gpuID)
	if lastGpuInfo != nil {
		eccInfo.DRAMErrors.Volatile.CorrectableDelta = getValueDelta(eccInfo.DRAMErrors.Volatile.Correctable, lastGpuInfo.ECC.DRAMErrors.Volatile.Correctable)
		eccInfo.DRAMErrors.Volatile.UncorrectableDelta = getValueDelta(eccInfo.DRAMErrors.Volatile.Uncorrectable, lastGpuInfo.ECC.DRAMErrors.Volatile.Uncorrectable)
		eccInfo.DRAMErrors.Aggregate.CorrectableDelta = getValueDelta(eccInfo.DRAMErrors.Aggregate.Correctable, lastGpuInfo.ECC.DRAMErrors.Aggregate.Correctable)
		eccInfo.DRAMErrors.Aggregate.UncorrectableDelta = getValueDelta(eccInfo.DRAMErrors.Aggregate.Uncorrectable, lastGpuInfo.ECC.DRAMErrors.Aggregate.Uncorrectable)

		eccInfo.SRAMErrors.Volatile.CorrectableDelta = getValueDelta(eccInfo.SRAMErrors.Volatile.Correctable, lastGpuInfo.ECC.SRAMErrors.Volatile.Correctable)
		eccInfo.SRAMErrors.Volatile.UncorrectableDelta = getValueDelta(eccInfo.SRAMErrors.Volatile.Uncorrectable, lastGpuInfo.ECC.SRAMErrors.Volatile.Uncorrectable)
		eccInfo.SRAMErrors.Aggregate.CorrectableDelta = getValueDelta(eccInfo.SRAMErrors.Aggregate.Correctable, lastGpuInfo.ECC.SRAMErrors.Aggregate.Correctable)
		eccInfo.SRAMErrors.Aggregate.UncorrectableDelta = getValueDelta(eccInfo.SRAMErrors.Aggregate.Uncorrectable, lastGpuInfo.ECC.SRAMErrors.Aggregate.Uncorrectable)
	}
}

// collectRowRemappingInfo collects GPU row remapping information.
// GetRemappedRows returns: corrRows, uncRows, isPending, failureOccurred, ret
//
// Pre-Ampere GPUs retire pages instead of remapping rows and answer
// NOT_SUPPORTED, which leaves Supported false without recording an error.
func (e *GpuMetricsExporter) collectRowRemappingInfo(device nvml.Device, gpuID string, info *gpumetrics.GPUInfo) {
	corrRows, uncRows, isPending, failureOccurred, ret := device.GetRemappedRows()
	switch ret {
	case nvml.SUCCESS:
	case nvml.ERROR_NOT_SUPPORTED:
		return
	default:
		info.RowRemapping.Error = fmt.Sprintf("device.GetRemappedRows error: %s", nvml.ErrorString(ret))
		return
	}

	info.RowRemapping.Supported = true
	info.RowRemapping.Correctable = uint64(corrRows)
	info.RowRemapping.Uncorrectable = uint64(uncRows)
	info.RowRemapping.Pending = isPending
	info.RowRemapping.Failed = failureOccurred

	lastGpuInfo := e.findLastGpuInfo(gpuID)
	if lastGpuInfo != nil {
		info.RowRemapping.CorrectableDelta = getValueDelta(info.RowRemapping.Correctable, lastGpuInfo.RowRemapping.Correctable)
		info.RowRemapping.UncorrectableDelta = getValueDelta(info.RowRemapping.Uncorrectable, lastGpuInfo.RowRemapping.Uncorrectable)
	}
}

// findLastNvLinkState returns the previous tick's state for one NVLink link, or
// nil if that link was not reported then.
//
// It matches on LinkIndex rather than on position in the slice. Now that links
// the GPU does not have are skipped, the two are no longer the same number, and
// indexing by position would diff a link against a different link's counters.
func findLastNvLinkState(lastGpuInfo *gpumetrics.GPUInfo, linkIndex int) *gpumetrics.NvLinkState {
	if lastGpuInfo == nil {
		return nil
	}

	for i := range lastGpuInfo.NvLink.Links {
		if lastGpuInfo.NvLink.Links[i].LinkIndex == linkIndex {
			return &lastGpuInfo.NvLink.Links[i]
		}
	}

	return nil
}

// collectNvLinkInfo enumerates the GPU's NVLink links and their error counters.
//
// NVLINK_MAX_LINKS is a header ceiling (18), not a link count: an A100 has 12
// and a card with no NVLink has none. Indices the GPU does not have answer
// NOT_SUPPORTED and are skipped rather than emitted, which is why entries carry
// their real index in LinkIndex. Counters that answer NOT_SUPPORTED are left
// out of Errors for the same reason — an inactive link reports none of them,
// and that is not a fault worth an error string on every tick.
func (e *GpuMetricsExporter) collectNvLinkInfo(device nvml.Device, gpuID string, info *gpumetrics.GPUInfo) {
	nvLinkInfo := gpumetrics.NvLinkInfo{}

	lastGpuInfo := e.findLastGpuInfo(gpuID)

	for linkIndex := 0; linkIndex < nvml.NVLINK_MAX_LINKS; linkIndex++ {
		state, ret := device.GetNvLinkState(linkIndex)
		if ret == nvml.ERROR_NOT_SUPPORTED || ret == nvml.ERROR_INVALID_ARGUMENT {
			// No such link on this GPU. Nothing to report.
			continue
		}

		linkState := gpumetrics.NvLinkState{
			LinkIndex:   linkIndex,
			Errors:      make(map[int]uint64),
			ErrorsDelta: make(map[int]int64),
		}

		if ret != nvml.SUCCESS {
			// A link that exists but could not be read: keep the entry, that
			// is the signal.
			linkState.Error = fmt.Sprintf("GetNvLinkState error: %s", nvml.ErrorString(ret))
			nvLinkInfo.Links = append(nvLinkInfo.Links, linkState)
			continue
		}

		nvLinkInfo.Supported = true

		switch state {
		case nvml.NVLINK_STATE_INACTIVE:
			linkState.State = "Inactive"
		case nvml.NVLINK_STATE_ACTIVE:
			linkState.State = "Active"
		case nvml.NVLINK_STATE_SLEEP:
			linkState.State = "Sleep"
		default:
			linkState.State = "Unknown"
		}

		lastLink := findLastNvLinkState(lastGpuInfo, linkIndex)

		for errCounter := nvml.NVLINK_ERROR_DL_REPLAY; errCounter < nvml.NVLINK_ERROR_COUNT; errCounter++ {
			value, ret := device.GetNvLinkErrorCounter(linkIndex, errCounter)
			switch ret {
			case nvml.SUCCESS:
				linkState.Errors[int(errCounter)] = value
				if lastLink != nil {
					if lastValue, ok := lastLink.Errors[int(errCounter)]; ok {
						linkState.ErrorsDelta[int(errCounter)] = getValueDelta(value, lastValue)
					}
				}
			case nvml.ERROR_NOT_SUPPORTED:
				// Absent from Errors already says the counter is unavailable.
			default:
				// First genuine failure wins: the five counters share one link,
				// so a real fault reads the same on all of them and the old
				// code's last-writer-wins left you reading "counter 4" while
				// counters 0..3 had failed just as hard.
				if linkState.Error == "" {
					linkState.Error = fmt.Sprintf("GetNvLinkErrorCounter %d error: %s", errCounter, nvml.ErrorString(ret))
				}
			}
		}

		nvLinkInfo.Links = append(nvLinkInfo.Links, linkState)
	}

	info.NvLink = nvLinkInfo
}

// Helper functions to parse throttle reasons
func parseThrottleReasons(reasons uint64) string {
	var parts []string

	if reasons&nvml.ClocksThrottleReasonGpuIdle != 0 {
		parts = append(parts, "GPU Idle")
	}
	if reasons&nvml.ClocksThrottleReasonApplicationsClocksSetting != 0 {
		parts = append(parts, "Applications Clocks Setting")
	}
	if reasons&nvml.ClocksThrottleReasonSwPowerCap != 0 {
		parts = append(parts, "SW Power Cap")
	}
	if reasons&nvml.ClocksThrottleReasonHwSlowdown != 0 {
		parts = append(parts, "HW Slowdown")
	}
	if reasons&nvml.ClocksThrottleReasonSyncBoost != 0 {
		parts = append(parts, "Sync Boost")
	}
	if reasons&nvml.ClocksThrottleReasonSwThermalSlowdown != 0 {
		parts = append(parts, "SW Thermal Slowdown")
	}
	if reasons&nvml.ClocksThrottleReasonHwThermalSlowdown != 0 {
		parts = append(parts, "HW Thermal Slowdown")
	}
	if reasons&nvml.ClocksThrottleReasonHwPowerBrakeSlowdown != 0 {
		parts = append(parts, "HW Power Brake Slowdown")
	}
	if reasons&nvml.ClocksThrottleReasonDisplayClockSetting != 0 {
		parts = append(parts, "Display Clock Setting")
	}

	if len(parts) == 0 {
		return "None"
	}
	return strings.Join(parts, ", ")
}

func parseEventReasons(events uint64) string {
	var parts []string

	if events&nvml.ClocksEventReasonGpuIdle != 0 {
		parts = append(parts, "GPU Idle")
	}
	if events&nvml.ClocksEventReasonApplicationsClocksSetting != 0 {
		parts = append(parts, "Applications Clocks Setting")
	}
	if events&nvml.ClocksEventReasonSwPowerCap != 0 {
		parts = append(parts, "SW Power Cap")
	}
	if events&nvml.ClocksEventReasonSyncBoost != 0 {
		parts = append(parts, "Sync Boost")
	}
	if events&nvml.ClocksEventReasonSwThermalSlowdown != 0 {
		parts = append(parts, "SW Thermal Slowdown")
	}
	if events&nvml.ClocksEventReasonDisplayClockSetting != 0 {
		parts = append(parts, "Display Clock Setting")
	}

	if len(parts) == 0 {
		return "None"
	}
	return strings.Join(parts, ", ")
}

func (e *GpuMetricsExporter) runCommand(name string, args ...string) (int, string) {
	execName := name
	execArgs := args
	if name == "systemctl" && e.config.HostRoot != "" && e.config.HostRoot != "/" {
		// If running in a container with host root mounted, run systemctl via host chroot
		execName = "chroot"
		execArgs = append([]string{e.config.HostRoot, "systemctl"}, args...)
	}
	e.log.Debug("run", zap.String("name", execName), zap.Strings("args", execArgs))
	cmd := exec.Command(execName, execArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode := exitError.Sys().(syscall.WaitStatus).ExitStatus()
			e.log.Warn("run", zap.String("name", execName), zap.Strings("args", execArgs), zap.Error(err),
				zap.Int("exitCode", exitCode), zap.String("output", string(output)))
			return exitCode, string(output)
		}
		e.log.Error("run", zap.String("name", execName), zap.Strings("args", execArgs), zap.Error(err), zap.String("output", string(output)))
		return -1, string(output)
	}
	e.log.Debug("run", zap.String("name", execName), zap.Strings("args", execArgs), zap.String("output", string(output)))
	return 0, string(output)
}

func (e *GpuMetricsExporter) checkFabricManager() (gpumetrics.NvFabricManagerStatus, error) {
	status := gpumetrics.NvFabricManagerStatus{}

	exitCode, output := e.runCommand("systemctl", "is-enabled", "nvidia-fabricmanager")
	status.Enabled = exitCode == 0 && strings.TrimSpace(output) == "enabled"

	exitCode, output = e.runCommand("systemctl", "is-active", "nvidia-fabricmanager")
	status.Active = exitCode == 0 && strings.TrimSpace(output) == "active"

	exitCode, output = e.runCommand("systemctl", "is-failed", "nvidia-fabricmanager")
	status.NotDegraded = exitCode == 1 && strings.TrimSpace(output) == "active"

	return status, nil
}

func (e *GpuMetricsExporter) getXIDErrors() ([]string, error) {
	window := e.config.TickPeriod + xidDmesgWindowSlack
	secAgo := int(window.Seconds())
	exitCode, output := e.runCommand("dmesg", "--since", fmt.Sprintf("%d sec ago", secAgo), "-l", "err")
	if exitCode != 0 {
		err := fmt.Errorf("dmesg failed with code: %d", exitCode)
		return nil, err
	}

	// Pull candidate XID/SXID lines out of dmesg without holding the lock: the
	// dmesg shell-out can take a while and must not block the send goroutine,
	// which needs lastMetricsMu to record send outcomes.
	var candidates []string
	for line := range strings.SplitSeq(output, "\n") {
		lowerLine := strings.ToLower(line)
		if strings.Contains(lowerLine, "xid") || strings.Contains(lowerLine, "sxid") {
			candidates = append(candidates, line)
		}
	}

	e.lastMetricsMu.Lock()
	defer e.lastMetricsMu.Unlock()

	// Retire records that can no longer collide with the dmesg window. This is
	// what keeps retiredXIDErrors bounded over the process lifetime: dmesg
	// lines carry timestamps, so no two are ever equal and an unpruned set
	// would grow forever.
	e.pruneRetiredXIDErrors(time.Now(), 2*window)

	// Buffer any fresh line: skip lines already retired (delivered, or dropped
	// on overflow) and lines already queued. Buffering happens regardless of
	// the dmesg window, so a line observed once is re-shipped on every
	// following payload until its carrying payload is confirmed sent —
	// surviving both transient send failures and dmesg-window expiry.
	for _, line := range candidates {
		if _, retired := e.retiredXIDErrors[line]; retired {
			continue
		}
		if slices.Contains(e.unsentXIDErrors, line) {
			continue
		}
		e.unsentXIDErrors = append(e.unsentXIDErrors, line)
	}

	e.trimUnsentXIDErrors()

	if len(e.unsentXIDErrors) == 0 {
		return nil, nil
	}
	// Return the full pending set; sendMetrics drains it on success.
	result := make([]string, len(e.unsentXIDErrors))
	copy(result, e.unsentXIDErrors)
	return result, nil
}

// trimUnsentXIDErrors bounds the pending re-ship buffer to maxUnsentXIDErrors.
// Left unbounded, the buffer grows the payload on every tick the host stays
// unreachable, and once the gzipped frame passes the 64 KiB vsock limit
// SendData fails on size permanently — the exporter would never recover, even
// after the host came back. Oldest lines go first (the newest describe the
// current fault) and the loss is counted into xidErrorsDropped so it reaches
// the host as XIDErrors.DroppedCount rather than vanishing.
//
// Callers must hold lastMetricsMu.
func (e *GpuMetricsExporter) trimUnsentXIDErrors() {
	overflow := len(e.unsentXIDErrors) - maxUnsentXIDErrors
	if overflow <= 0 {
		return
	}

	dropped := e.unsentXIDErrors[:overflow]
	e.unsentXIDErrors = append([]string(nil), e.unsentXIDErrors[overflow:]...)
	e.xidErrorsDropped += int64(overflow)

	// Retire the evicted lines so the next tick cannot re-admit them. Without
	// this they are neither retired nor pending, so a line still inside the
	// overlapping dmesg window looks fresh, gets appended to the *tail*, and
	// the following trim evicts from the head — which by then holds lines
	// *newer* than the ones just re-admitted. That inverts the oldest-first
	// rule above and re-counts the same physical line into xidErrorsDropped on
	// every tick it thrashes, making DroppedCount overstate the real loss.
	if e.retiredXIDErrors == nil {
		e.retiredXIDErrors = make(map[string]time.Time, overflow)
	}
	now := time.Now()
	for _, line := range dropped {
		e.retiredXIDErrors[line] = now
	}
	e.log.Warn("XID buffer full, dropped oldest lines",
		zap.Int("dropped", overflow),
		zap.Int("max", maxUnsentXIDErrors),
		zap.Int64("droppedTotal", e.xidErrorsDropped),
		zap.Strings("droppedLines", dropped))
}

// pruneRetiredXIDErrors drops retired-line records older than retention, then
// trims what remains to the maxRetiredXIDErrors most recent as a backstop against
// a dmesg window carrying more distinct lines than age alone retires.
//
// Evicting a record whose line is still inside the dmesg look-back window lets
// that line be re-admitted to the pending buffer. Callers pass a retention of
// twice the window, so age-based pruning never does this; only the size
// backstop can, and it trips solely during an XID storm where a re-admission is
// the lesser problem.
//
// Callers must hold lastMetricsMu.
func (e *GpuMetricsExporter) pruneRetiredXIDErrors(now time.Time, retention time.Duration) {
	for line, retiredAt := range e.retiredXIDErrors {
		if now.Sub(retiredAt) > retention {
			delete(e.retiredXIDErrors, line)
		}
	}

	if len(e.retiredXIDErrors) <= maxRetiredXIDErrors {
		return
	}

	lines := make([]string, 0, len(e.retiredXIDErrors))
	for line := range e.retiredXIDErrors {
		lines = append(lines, line)
	}
	// Newest first, so everything past the cap is the oldest.
	slices.SortFunc(lines, func(a, b string) int {
		return e.retiredXIDErrors[b].Compare(e.retiredXIDErrors[a])
	})
	for _, line := range lines[maxRetiredXIDErrors:] {
		delete(e.retiredXIDErrors, line)
	}
}

// xidErrorsDroppedCount returns the cumulative number of XID/SXID lines
// discarded because the pending re-ship buffer overflowed.
func (e *GpuMetricsExporter) xidErrorsDroppedCount() int64 {
	e.lastMetricsMu.RLock()
	defer e.lastMetricsMu.RUnlock()
	return e.xidErrorsDropped
}

// collectMIGInfo collects the GPU's Multi-Instance GPU state and, when MIG is
// active, one entry per instantiated MIG device.
//
// GetMigMode answering NOT_SUPPORTED is the normal case on non-MIG hardware, so
// it leaves Supported false rather than recording an error — only a genuine
// failure on a MIG-capable card is worth surfacing. Everything else follows the
// house rule: an unsupported sub-query no-ops and leaves its field zero-valued
// instead of failing the tick.
func (e *GpuMetricsExporter) collectMIGInfo(device nvml.Device, info *gpumetrics.GPUInfo) {
	currentMode, pendingMode, ret := device.GetMigMode()
	if ret == nvml.ERROR_NOT_SUPPORTED {
		// Not a MIG-capable GPU. Supported stays false; this is not an error.
		return
	}
	if ret != nvml.SUCCESS {
		info.MIG.Error = fmt.Sprintf("GetMigMode error: %s", nvml.ErrorString(ret))
		return
	}

	info.MIG.Supported = true
	info.MIG.Enabled = currentMode == nvml.DEVICE_MIG_ENABLE
	info.MIG.PendingEnabled = pendingMode == nvml.DEVICE_MIG_ENABLE
	// A pending mode that differs from the current one means the partitioning
	// an operator configured is not the one running: it takes effect only after
	// a GPU reset (or once every client releases the device).
	info.MIG.PendingChange = currentMode != pendingMode

	if !info.MIG.Enabled {
		// With MIG off there are no instances to walk, and the handle queries
		// below would fail on every index.
		return
	}

	maxCount, ret := device.GetMaxMigDeviceCount()
	if ret != nvml.SUCCESS {
		info.MIG.Error = fmt.Sprintf("GetMaxMigDeviceCount error: %s", nvml.ErrorString(ret))
		return
	}

	for i := 0; i < maxCount; i++ {
		migDevice, ret := device.GetMigDeviceHandleByIndex(i)
		if ret == nvml.ERROR_NOT_FOUND {
			// Sparse by design: indices below maxCount need not be populated,
			// because a partitioning leaves gaps (e.g. two 3g.40gb instances on
			// a 7-slice A100). An empty slot is not an error.
			continue
		}
		if ret != nvml.SUCCESS {
			info.MIG.Instances = append(info.MIG.Instances, gpumetrics.MIGInstance{
				Index: i,
				Error: fmt.Sprintf("GetMigDeviceHandleByIndex error: %s", nvml.ErrorString(ret)),
			})
			continue
		}

		info.MIG.Instances = append(info.MIG.Instances, e.collectMIGInstance(migDevice, i))
	}

	info.MIG.InstanceCount = len(info.MIG.Instances)
}

// collectMIGInstance reads one MIG device handle. A MIG handle answers only a
// subset of the device API, so each query is guarded individually and a failure
// leaves that field zero-valued rather than dropping the whole instance.
func (e *GpuMetricsExporter) collectMIGInstance(migDevice nvml.Device, index int) gpumetrics.MIGInstance {
	instance := gpumetrics.MIGInstance{Index: index}

	if uuid, ret := migDevice.GetUUID(); ret == nvml.SUCCESS {
		instance.UUID = uuid
	}
	if name, ret := migDevice.GetName(); ret == nvml.SUCCESS {
		instance.Name = name
	}
	if gpuInstanceID, ret := migDevice.GetGpuInstanceId(); ret == nvml.SUCCESS {
		instance.GpuInstanceID = gpuInstanceID
	}
	if computeInstanceID, ret := migDevice.GetComputeInstanceId(); ret == nvml.SUCCESS {
		instance.ComputeInstanceID = computeInstanceID
	}
	if memInfo, ret := migDevice.GetMemoryInfo(); ret == nvml.SUCCESS {
		instance.MemoryTotal = memInfo.Total
		instance.MemoryUsed = memInfo.Used
		instance.MemoryFree = memInfo.Free
	}
	// Attributes carry the partition's size: how many slices of the parent GPU
	// this instance owns, and how many SMs that works out to.
	if attrs, ret := migDevice.GetAttributes(); ret == nvml.SUCCESS {
		instance.MultiprocessorCount = uint(attrs.MultiprocessorCount)
		instance.GpuInstanceSliceCount = uint(attrs.GpuInstanceSliceCount)
		instance.ComputeInstanceSliceCount = uint(attrs.ComputeInstanceSliceCount)
	}

	return instance
}
