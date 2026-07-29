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
	data, err := os.ReadFile(cloudInitInstanceIDFilePath)
	if err != nil {
		return "", err
	}
	instanceID := strings.TrimSpace(string(data))
	return instanceID, nil
}

// initNVML initializes the NVML library
func (e *GpuMetricsExporter) initNVML() error {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		err := fmt.Errorf("nvml init error %s", nvml.ErrorString(ret))
		e.log.Error("nvml.Init", zap.Error(err))
		return err
	}
	return nil
}

// shutdownNVML shuts down the NVML library
func (e *GpuMetricsExporter) shutdownNVML() {
	ret := nvml.Shutdown()
	if ret != nvml.SUCCESS {
		err := fmt.Errorf("nvml shutdown error %s", nvml.ErrorString(ret))
		e.log.Error("nvml.Shutdown", zap.Error(err))
	}
}

// getDeviceCount returns the number of NVIDIA GPUs
func (e *GpuMetricsExporter) getDeviceCount() (int, error) {
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		err := fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
		e.log.Error("nvml.DeviceGetCount", zap.Error(err))
		return 0, err
	}
	return count, nil
}

// getDeviceHandle returns the device handle for a given GPU index
func (e *GpuMetricsExporter) getDeviceHandle(index int) (nvml.Device, error) {
	device, ret := nvml.DeviceGetHandleByIndex(index)
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
	e.collectBAR1MemoryInfo(device, &gpuInfo) // NEW
	e.collectUtilization(device, &gpuInfo)
	e.collectDecoderEncoderUtil(device, &gpuInfo) // NEW
	e.collectClockSpeeds(device, &gpuInfo)
	e.collectFanSpeed(device, &gpuInfo)
	e.collectPCIInfo(device, &gpuInfo)
	e.collectDriverInfo(&gpuInfo)
	e.collectVBiosInfo(device, &gpuInfo)
	e.collectArchitecture(device, &gpuInfo) // NEW
	e.collectCUDACores(device, &gpuInfo)    // NEW
	e.collectComputeMode(device, &gpuInfo)
	e.collectPerformanceState(device, &gpuInfo)
	e.collectPersistenceMode(device, &gpuInfo)
	e.collectClocksThrottleInfo(device, &gpuInfo) // NEW
	e.collectECCInfo(device, gpuInfo.UUID, &gpuInfo)
	e.collectRowRemappingInfo(device, gpuInfo.UUID, &gpuInfo)
	e.collectNvLinkInfo(device, gpuInfo.UUID, &gpuInfo) // NEW

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

	// Get memory temperature using field values
	fieldValues := []nvml.FieldValue{
		{FieldId: nvml.FI_DEV_MEMORY_TEMP},
	}

	if ret := device.GetFieldValues(fieldValues); ret == nvml.SUCCESS {
		if len(fieldValues) > 0 {
			// Memory temperature is typically stored as uint32 in the byte array
			info.MemoryTemp = uint(fieldValues[0].Value[0]) |
				uint(fieldValues[0].Value[1])<<8 |
				uint(fieldValues[0].Value[2])<<16 |
				uint(fieldValues[0].Value[3])<<24
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

// NEW: collectBAR1MemoryInfo collects BAR1 memory information
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

// NEW: collectDecoderEncoderUtil collects decoder and encoder utilization
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
	if ret == nvml.SUCCESS {
		info.FanSpeed = uint(fanSpeed)
	} else if ret == nvml.ERROR_NOT_SUPPORTED {
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
	if driverVersion, ret := nvml.SystemGetDriverVersion(); ret == nvml.SUCCESS {
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

// NEW: collectArchitecture collects GPU architecture information
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

// NEW: collectClocksThrottleInfo collects clocks throttle reasons
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

// collectECCInfo collects GPU ECC information and errors (enhanced)
func (e *GpuMetricsExporter) collectECCInfo(device nvml.Device, gpuID string, info *gpumetrics.GPUInfo) {
	currentMode, pendingMode, ret := device.GetEccMode()
	if ret != nvml.SUCCESS {
		return
	}

	info.ECC.Enabled = currentMode == 1
	info.ECC.Pending = pendingMode == 1

	if currentMode == 1 {
		info.ECC.Mode = "Enabled"
		e.collectECCErrors(device, gpuID, &info.ECC)
		e.collectRetiredPages(device, gpuID, &info.ECC) // NEW
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

// More detailed retired pages collection
func (e *GpuMetricsExporter) collectRetiredPages(device nvml.Device, gpuID string, eccInfo *gpumetrics.ECCInfo) {
	fieldValues := []nvml.FieldValue{
		{FieldId: nvml.FI_DEV_RETIRED_SBE},
		{FieldId: nvml.FI_DEV_RETIRED_DBE},
		{FieldId: nvml.FI_DEV_RETIRED_PENDING},
	}

	if ret := device.GetFieldValues(fieldValues); ret == nvml.SUCCESS {
		if len(fieldValues) >= 3 {
			// Assuming little-endian byte order (common for x86)
			eccInfo.RetiredPages.SBEPages = binary.LittleEndian.Uint64(fieldValues[0].Value[:])
			eccInfo.RetiredPages.DBEPages = binary.LittleEndian.Uint64(fieldValues[1].Value[:])
			eccInfo.RetiredPages.PendingPages = binary.LittleEndian.Uint64(fieldValues[2].Value[:])

			lastGpuInfo := e.findLastGpuInfo(gpuID)
			if lastGpuInfo != nil {
				eccInfo.RetiredPages.SBEPagesDelta = getValueDelta(eccInfo.RetiredPages.SBEPages, lastGpuInfo.ECC.RetiredPages.SBEPages)
				eccInfo.RetiredPages.DBEPagesDelta = getValueDelta(eccInfo.RetiredPages.DBEPages, lastGpuInfo.ECC.RetiredPages.DBEPages)
				eccInfo.RetiredPages.PendingPagesDelta = getValueDelta(eccInfo.RetiredPages.PendingPages, lastGpuInfo.ECC.RetiredPages.PendingPages)
			}
		} else {
			eccInfo.RetiredPages.Error = fmt.Sprintf("len(fieldValues): %d != 3", len(fieldValues))
		}
	} else {
		eccInfo.RetiredPages.Error = fmt.Sprintf("GetFieldValues error: %s", nvml.ErrorString(ret))
	}
}

// collectECCErrors collects ECC error counts (both volatile and aggregate)
func (e *GpuMetricsExporter) collectECCErrorsInternal(device nvml.Device, memoryLocation nvml.MemoryLocation, result *gpumetrics.ECCErrorsCounters) {
	// Device Memory (DRAM) - Volatile correctable errors (since last reset)
	errors, ret := device.GetMemoryErrorCounter(
		nvml.MEMORY_ERROR_TYPE_CORRECTED,
		nvml.VOLATILE_ECC,
		memoryLocation,
	)
	if ret == nvml.SUCCESS {
		result.Volatile.Correctable = errors
	} else {
		e.log.Warn("Failed to get volatile ECC Correctable",
			zap.String("error", nvml.ErrorString(ret)), zap.Int32("memoryLocation", int32(memoryLocation)))

		result.Volatile.Error = fmt.Sprintf("failed to get volatile ECC correctable: %s", nvml.ErrorString(ret))
	}

	// Device Memory (DRAM) - Aggregate correctable errors (lifetime)
	errors, ret = device.GetMemoryErrorCounter(
		nvml.MEMORY_ERROR_TYPE_CORRECTED,
		nvml.AGGREGATE_ECC,
		memoryLocation,
	)
	if ret == nvml.SUCCESS {
		result.Aggregate.Correctable = errors
	} else {
		e.log.Warn("Failed to get aggregate ECC Correctable",
			zap.String("error", nvml.ErrorString(ret)), zap.Int32("memoryLocation", int32(memoryLocation)))

		result.Aggregate.Error = fmt.Sprintf("failed to get aggregate ECC correctable: %s", nvml.ErrorString(ret))
	}

	// Device Memory (DRAM) - Volatile uncorrectable errors
	errors, ret = device.GetMemoryErrorCounter(
		nvml.MEMORY_ERROR_TYPE_UNCORRECTED,
		nvml.VOLATILE_ECC,
		memoryLocation,
	)
	if ret == nvml.SUCCESS {
		result.Volatile.Uncorrectable = errors
	} else {
		e.log.Warn("Failed to get volatile ECC Uncorrectable",
			zap.String("error", nvml.ErrorString(ret)), zap.Int32("memoryLocation", int32(memoryLocation)))

		result.Volatile.Error = fmt.Sprintf("failed to get volatile ECC uncorrectable: %s", nvml.ErrorString(ret))
	}

	// Device Memory (DRAM) - Aggregate uncorrectable errors (lifetime)
	errors, ret = device.GetMemoryErrorCounter(
		nvml.MEMORY_ERROR_TYPE_UNCORRECTED,
		nvml.AGGREGATE_ECC,
		memoryLocation,
	)
	if ret == nvml.SUCCESS {
		result.Aggregate.Uncorrectable = errors
	} else {
		e.log.Warn("Failed to get aggregate ECC Uncorrectable",
			zap.String("error", nvml.ErrorString(ret)), zap.Int32("memoryLocation", int32(memoryLocation)))

		result.Aggregate.Error = fmt.Sprintf("failed to get aggregate ECC uncorrectable: %s", nvml.ErrorString(ret))
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

// collectRowRemappingInfo collects GPU row remapping information
// GetRemappedRows returns: corrRows, uncRows, isPending, failureOccurred, ret
func (e *GpuMetricsExporter) collectRowRemappingInfo(device nvml.Device, gpuID string, info *gpumetrics.GPUInfo) {
	corrRows, uncRows, isPending, failureOccurred, ret := device.GetRemappedRows()
	if ret != nvml.SUCCESS {
		info.RowRemapping.Error = fmt.Sprintf("device.GetRemappedRows error: %s", nvml.ErrorString(ret))
		return
	}

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

// collectNvLinkInfo collects NVLink information
func (e *GpuMetricsExporter) collectNvLinkInfo(device nvml.Device, gpuID string, info *gpumetrics.GPUInfo) {
	nvLinkInfo := gpumetrics.NvLinkInfo{}

	lastGpuInfo := e.findLastGpuInfo(gpuID)

	// Try to get NVLink info - this may not be supported on all GPUs
	for linkIndex := 0; linkIndex < nvml.NVLINK_MAX_LINKS; linkIndex++ {
		linkState := gpumetrics.NvLinkState{LinkIndex: linkIndex, Errors: make(map[int]uint64), ErrorsDelta: make(map[int]int64)}

		// Check if this link is active
		state, ret := device.GetNvLinkState(linkIndex)
		if ret == nvml.SUCCESS {
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

			for errCounter := nvml.NVLINK_ERROR_DL_REPLAY; errCounter < nvml.NVLINK_ERROR_COUNT; errCounter++ {
				value, ret := device.GetNvLinkErrorCounter(linkIndex, errCounter)
				if ret == nvml.SUCCESS {
					currValue := uint64(value)
					linkState.Errors[int(errCounter)] = currValue
					if lastGpuInfo != nil {
						if linkIndex < len(lastGpuInfo.NvLink.Links) {
							lastValue, ok := lastGpuInfo.NvLink.Links[linkIndex].Errors[int(errCounter)]
							if ok {
								linkState.ErrorsDelta[int(errCounter)] = getValueDelta(currValue, lastValue)
							}
						}
					}
				} else {
					linkState.Error = fmt.Sprintf("GetNvLinkErrorCounter %d error: %s", errCounter, nvml.ErrorString(ret))
				}
			}
		} else {
			linkState.Error = fmt.Sprintf("GetNvLinkState error: %s", nvml.ErrorString(ret))
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
	e.log.Info("run", zap.String("name", name), zap.Strings("args", args))
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode := exitError.Sys().(syscall.WaitStatus).ExitStatus()
			e.log.Warn("run", zap.String("name", name), zap.Strings("args", args), zap.Error(err),
				zap.Int("exitCode", exitCode), zap.String("output", string(output)))
			return exitCode, string(output)
		}
		e.log.Error("run", zap.String("name", name), zap.Strings("args", args), zap.Error(err), zap.String("output", string(output)))
		return -1, string(output)
	}
	e.log.Info("run", zap.String("name", name), zap.Strings("args", args), zap.String("output", string(output)))
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
	exitCode, output := e.runCommand("dmesg", "--since", fmt.Sprintf("%d sec ago", e.config.TickPeriod+10), "-l", "err")
	if exitCode != 0 {
		err := fmt.Errorf("dmesg failed with code: %d", exitCode)
		return nil, err
	}

	var errors []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		lowerLine := strings.ToLower(line)
		if strings.Contains(lowerLine, "xid") || strings.Contains(lowerLine, "sxid") {
			// do not send already sent dmesg lines
			e.lastMetricsMu.RLock()
			if e.lastMetrics != nil && slices.Contains(e.lastMetrics.XIDErrors.XIDErrors, line) {
				e.lastMetricsMu.RUnlock()
				continue
			}
			e.lastMetricsMu.RUnlock()

			errors = append(errors, line)
		}
	}

	return errors, nil
}
