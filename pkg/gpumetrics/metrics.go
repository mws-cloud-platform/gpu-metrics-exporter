package gpumetrics

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
)

// GpuMetricsSource identifies where a metrics payload originated: the guest
// VM's vsock CID (filled in by the receiver from the peer address) and its
// cloud-init instance ID (read by the exporter). Downstream code is expected
// to map VsockClientID -> VMID.
type GpuMetricsSource struct {
	VsockClientID uint32 `json:"vsock_client_id"`
	InstanceID    string `json:"instance_id"`
}

// CUDAComputeCapability is the major.minor CUDA compute capability of a GPU.
type CUDAComputeCapability struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

// GPUInfo represents the complete information we want to collect about a GPU
type GPUInfo struct {
	Index                 int                   `json:"index"`
	Error                 string                `json:"error"`
	UUID                  string                `json:"uuid"`
	Name                  string                `json:"name"`
	Serial                string                `json:"serial"`
	Temperature           uint                  `json:"temperature_celsius"`
	MemoryTemp            uint                  `json:"memory_temperature_celsius"`
	PowerUsage            uint                  `json:"power_usage_watts"`
	PowerLimit            uint                  `json:"power_limit_watts"`
	MemoryTotal           uint64                `json:"memory_total_bytes"`
	MemoryUsed            uint64                `json:"memory_used_bytes"`
	MemoryFree            uint64                `json:"memory_free_bytes"`
	BAR1MemoryTotal       uint64                `json:"bar1_memory_total_bytes"`
	BAR1MemoryUsed        uint64                `json:"bar1_memory_used_bytes"`
	BAR1MemoryFree        uint64                `json:"bar1_memory_free_bytes"`
	Utilization           GPUUtilization        `json:"utilization"`
	DecoderUtil           uint                  `json:"decoder_utilization_percent"`
	EncoderUtil           uint                  `json:"encoder_utilization_percent"`
	Clocks                GPUClocks             `json:"clocks"`
	FanSpeed              uint                  `json:"fan_speed_percent"`
	PCI                   PCIInfo               `json:"pci_info"`
	DriverModel           string                `json:"driver_model"`
	DriverVersion         string                `json:"driver_version"`
	VBios                 string                `json:"vbios_version"`
	ComputeMode           string                `json:"compute_mode"`
	ComputeModeValue      int                   `json:"compute_mode_value"`
	PerformanceState      string                `json:"performance_state"`
	PerformanceStateValue int                   `json:"performance_state_value"`
	ECC                   ECCInfo               `json:"ecc_info"`
	RowRemapping          RowRemappingInfo      `json:"row_remapping_info"`
	ClocksThrottle        ClocksThrottleInfo    `json:"clocks_throttle_info"`
	NvLink                NvLinkInfo            `json:"nvlink_info"`
	Architecture          string                `json:"architecture"`
	CUDAComputeCapability CUDAComputeCapability `json:"cuda_compute_capability"`
	PersistenceMode       int                   `json:"persistence_mode"`
	MIG                   MIGInfo               `json:"mig_info"`
}

// MIGInfo describes a GPU's Multi-Instance GPU partitioning.
//
// Supported is false on GPUs where NVML reports MIG as unsupported at all
// (anything pre-Ampere, and Ampere+ cards whose driver refuses the query); the
// remaining fields are meaningless then. Enabled reflects the mode in force
// now, PendingEnabled the mode NVML will apply once the GPU is reset or all
// clients release it, and PendingChange is the actionable difference: the two
// disagree, so the partitioning an operator asked for is not the one running.
//
// Reading Enabled matters beyond MIG itself: with MIG on, NVML answers many
// whole-device queries — utilization, per-device ECC volatile counters, clocks
// — with NOT_SUPPORTED. The collectors treat that as "leave zero-valued", so a
// MIG-enabled GPU reports zeros that a consumer would otherwise read as a genuinely
// idle card. mig_info.enabled is how to tell those two apart.
type MIGInfo struct {
	Supported      bool          `json:"supported"`
	Enabled        bool          `json:"enabled"`
	PendingEnabled bool          `json:"pending_enabled"`
	PendingChange  bool          `json:"pending_change"`
	InstanceCount  int           `json:"instance_count"`
	Instances      []MIGInstance `json:"instances"`
	Error          string        `json:"error"`
}

// MIGInstance is one instantiated MIG device: a compute instance within a GPU
// instance, which NVML addresses through its own device handle.
//
// GpuInstanceID and ComputeInstanceID are the identifiers NVML and nvidia-smi
// use to name the partition, and together with the parent GPU's UUID they
// identify it across ticks. SliceCount fields give the partition's size in the
// card's slice units (e.g. 3 of 7 on an A100 3g.40gb).
type MIGInstance struct {
	Index                     int    `json:"index"`
	UUID                      string `json:"uuid"`
	Name                      string `json:"name"`
	GpuInstanceID             int    `json:"gpu_instance_id"`
	ComputeInstanceID         int    `json:"compute_instance_id"`
	MemoryTotal               uint64 `json:"memory_total_bytes"`
	MemoryUsed                uint64 `json:"memory_used_bytes"`
	MemoryFree                uint64 `json:"memory_free_bytes"`
	MultiprocessorCount       uint   `json:"multiprocessor_count"`
	GpuInstanceSliceCount     uint   `json:"gpu_instance_slice_count"`
	ComputeInstanceSliceCount uint   `json:"compute_instance_slice_count"`
	Error                     string `json:"error"`
}

// GPUUtilization holds current GPU and memory utilization as percentages.
type GPUUtilization struct {
	GPU    uint `json:"gpu_percent"`
	Memory uint `json:"memory_percent"`
}

// GPUClocks holds the current graphics, memory and SM clock speeds in MHz.
type GPUClocks struct {
	Graphics uint `json:"graphics_mhz"`
	Memory   uint `json:"memory_mhz"`
	SM       uint `json:"sm_mhz"`
}

// PCIInfo describes the GPU's PCI bus location, current and maximum link
// capabilities, and live RX/TX throughput.
type PCIInfo struct {
	BusID        string `json:"bus_id"`
	Domain       uint   `json:"domain"`
	Bus          uint   `json:"bus"`
	Device       uint   `json:"device"`
	PCIGen       uint   `json:"pci_generation"`
	LinkWidth    uint   `json:"link_width_current"`
	MaxPCIGen    uint   `json:"max_pci_generation"`
	MaxLinkWidth uint   `json:"max_link_width"`
	TxThroughput uint32 `json:"tx_throughput"`
	RxThroughput uint32 `json:"rx_throughput"`
}

// ECCInfo holds the GPU's ECC mode and error counters for DRAM/SRAM memory and
// retired pages. Delta fields carry the per-tick change since the last report.
type ECCInfo struct {
	Enabled      bool              `json:"enabled"`
	Pending      bool              `json:"pending"`
	Mode         string            `json:"mode"`
	DRAMErrors   ECCErrorsCounters `json:"dram_errors"`
	SRAMErrors   ECCErrorsCounters `json:"sram_errors"`
	RetiredPages RetiredPagesInfo  `json:"retired_pages"`
}

// ECCErrors holds correctable and uncorrectable ECC error counts for one
// counter scope (volatile or aggregate), plus their per-tick deltas.
type ECCErrors struct {
	Correctable        uint64 `json:"correctable"`
	Uncorrectable      uint64 `json:"uncorrectable"`
	CorrectableDelta   int64  `json:"correctable_delta"`
	UncorrectableDelta int64  `json:"uncorrectable_delta"`
	Error              string `json:"error"`
}

// ECCErrorsCounters splits ECC errors into volatile (since last reset) and
// aggregate (lifetime) scopes.
type ECCErrorsCounters struct {
	Volatile  ECCErrors `json:"volatile"`
	Aggregate ECCErrors `json:"aggregate"`
}

// RetiredPagesInfo holds counts of retired GPU memory pages due to ECC errors,
// with per-tick deltas. SBE = single-bit, DBE = double-bit.
type RetiredPagesInfo struct {
	SBEPages          uint64 `json:"sbe_pages"`
	DBEPages          uint64 `json:"dbe_pages"`
	SBEPagesDelta     int64  `json:"sbe_pages_delta"`
	DBEPagesDelta     int64  `json:"dbe_pages_delta"`
	PendingPages      uint64 `json:"pending_pages"`
	PendingPagesDelta int64  `json:"pending_pages_delta"`
	Error             string `json:"error"`
}

// RowRemappingInfo holds row remapping state for GPUs that support it, with
// per-tick deltas of corrected and uncorrected remapped rows.
type RowRemappingInfo struct {
	Pending            bool   `json:"pending"`
	Failed             bool   `json:"failed"`
	Correctable        uint64 `json:"correctable"`
	Uncorrectable      uint64 `json:"uncorrectable"`
	CorrectableDelta   int64  `json:"correctable_delta"`
	UncorrectableDelta int64  `json:"uncorrectable_delta"`
	Error              string `json:"error"`
}

// ClocksThrottleInfo holds the GPU's current clocks throttle and event reasons
// (bitmasks) plus their human-readable forms.
type ClocksThrottleInfo struct {
	ThrottleReasons    uint64 `json:"throttle_reasons"`
	ThrottleReasonsStr string `json:"throttle_reasons_string"`
	EventReasons       uint64 `json:"event_reasons"`
	EventReasonsStr    string `json:"event_reasons_string"`
}

// NvLinkInfo holds per-link NVLink state and error counters for a GPU.
type NvLinkInfo struct {
	Links []NvLinkState `json:"links"`
}

// NvLinkState holds the state and per-error-type counters for a single NVLink
// link, with per-tick deltas keyed by NVML error counter type.
type NvLinkState struct {
	LinkIndex   int            `json:"index"`
	State       string         `json:"state"`
	Errors      map[int]uint64 `json:"errors"`
	ErrorsDelta map[int]int64  `json:"errors_delta"`
	Error       string         `json:"error"`
}

// ExporterInfo carries exporter process metadata and per-tick health: a
// monotonically increasing sequence number, a timestamp, the build version, the
// process start time, and any NVML/read/send errors observed so far.
type ExporterInfo struct {
	Seqno                 int64  `json:"seqno"`
	Timestamp             int64  `json:"timestamp"`
	Version               string `json:"version"`
	StartTime             int64  `json:"start_time"`
	InitNVMLError         string `json:"init_nvml_error"`
	GetDeviceCountError   string `json:"get_device_count_error"`
	ReadInstanceIDError   string `json:"read_instance_id_error"`
	SendMetricsErrorCount int64  `json:"send_metrics_error_count"`
	SendMetricsLastError  string `json:"send_metrics_last_error"`
}

// NvFabricManagerStatus reports the health of the nvidia-fabricmanager service
// (relevant for multi-GPU NVLink fabrics such as H100).
type NvFabricManagerStatus struct {
	Error       string `json:"error"`
	Active      bool   `json:"active"`
	Enabled     bool   `json:"enabled"`
	NotDegraded bool   `json:"not_degraded"`
}

// XIDErrors holds recent XID/SXID error lines collected from the kernel log.
// A line is re-shipped on every payload until one carrying it is confirmed
// delivered, so a transient send failure cannot lose an error. DroppedCount is
// the cumulative number of lines the exporter had to discard because that
// pending buffer overflowed — non-zero means XID errors were lost, and the
// kernel log on the guest is the only remaining record of them.
type XIDErrors struct {
	Error        string   `json:"error"`
	XIDErrors    []string `json:"xid_errors"`
	DroppedCount int64    `json:"dropped_count"`
}

// GpuMetrics is the top-level payload exchanged between the guest exporter and
// the host receiver: provenance, exporter health, fabric-manager status, XID
// errors, and one GPUInfo entry per detected GPU.
type GpuMetrics struct {
	// WireVersion is the format version the *exporter* spoke, not the shape of
	// this struct: NewGpuMetricsFromBytes always returns the current shape,
	// upconverting older payloads into it. So a value below CurrentWireVersion
	// means some fields could not possibly have been populated — a v1 exporter
	// knows nothing of mig_info or xid_errors.dropped_count, and their zero
	// values mean "not reported", not "reported as zero". Consumers that care
	// about the difference must check this.
	WireVersion           int                   `json:"wire_version"`
	Source                GpuMetricsSource      `json:"source"`
	ExporterInfo          ExporterInfo          `json:"exporter_info"`
	NvFabricManagerStatus NvFabricManagerStatus `json:"nv_fabric_manager_status"`
	XIDErrors             XIDErrors             `json:"xid_errors"`
	GpuDeviceCount        int                   `json:"gpu_device_count"`
	Gpus                  []GPUInfo             `json:"gpu_info"`
}

// NewGpuMetrics returns a zero-value GpuMetrics ready to be populated.
func NewGpuMetrics() *GpuMetrics {
	m := &GpuMetrics{WireVersion: CurrentWireVersion}
	return m
}

// compressJSON marshals v to JSON and gzip-compresses the result.
func compressJSON(data interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(jsonData); err != nil {
		_ = writer.Close()
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
