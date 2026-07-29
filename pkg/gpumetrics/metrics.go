package gpumetrics

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
)

// GpuMetricsSouce identifies where a metrics payload originated: the guest
// VM's vsock CID (filled in by the receiver from the peer address) and its
// cloud-init instance ID (read by the exporter). Downstream code is expected
// to map VsockClientID -> VMID.
type GpuMetricsSouce struct {
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
	MemoryTemp            uint                  `json:"memory_temperature_celsius"` // NEW
	PowerUsage            uint                  `json:"power_usage_watts"`
	PowerLimit            uint                  `json:"power_limit_watts"`
	MemoryTotal           uint64                `json:"memory_total_bytes"`
	MemoryUsed            uint64                `json:"memory_used_bytes"`
	MemoryFree            uint64                `json:"memory_free_bytes"`
	BAR1MemoryTotal       uint64                `json:"bar1_memory_total_bytes"` // NEW
	BAR1MemoryUsed        uint64                `json:"bar1_memory_used_bytes"`  // NEW
	BAR1MemoryFree        uint64                `json:"bar1_memory_free_bytes"`  // NEW
	Utilization           GPUUtilization        `json:"utilization"`
	DecoderUtil           uint                  `json:"decoder_utilization_percent"` // NEW
	EncoderUtil           uint                  `json:"encoder_utilization_percent"` // NEW
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
	ClocksThrottle        ClocksThrottleInfo    `json:"clocks_throttle_info"`    // NEW
	NvLink                NvLinkInfo            `json:"nvlink_info"`             // NEW
	Architecture          string                `json:"architecture"`            // NEW
	CUDAComputeCapability CUDAComputeCapability `json:"cuda_compute_capability"` // NEW
	PersistenceMode       int                   `json:"persistence_mode"`
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
	MaxPCIGen    uint   `json:"max_pci_generation"` // NEW
	MaxLinkWidth uint   `json:"max_link_width"`     // NEW
	TxThroughput uint32 `json:"tx_throughtput"`
	RxThroughput uint32 `json:"rx_throughtput"`
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
	SBEPages          uint64 `json:"sbe_pages"` // NEW
	DBEPages          uint64 `json:"dbe_pages"` // NEW
	SBEPagesDelta     int64  `json:"sbe_pages_delta"`
	DBEPagesDelta     int64  `json:"dbe_pages_delta"`
	PendingPages      uint64 `json:"pending_pages"` // NEW
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

// NEW: Clocks throttle information
type ClocksThrottleInfo struct {
	ThrottleReasons    uint64 `json:"throttle_reasons"`
	ThrottleReasonsStr string `json:"throttle_reasons_string"`
	EventReasons       uint64 `json:"event_reasons"`
	EventReasonsStr    string `json:"event_reasons_string"`
}

// NEW: NVLink information
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
type XIDErrors struct {
	Error     string   `json:"error"`
	XIDErrors []string `json:"xid_errors"`
}

// GpuMetrics is the top-level payload exchanged between the guest exporter and
// the host receiver: provenance, exporter health, fabric-manager status, XID
// errors, and one GPUInfo entry per detected GPU.
type GpuMetrics struct {
	Source                GpuMetricsSouce       `json:"source"`
	ExporterInfo          ExporterInfo          `json:"exporter_info"`
	NvFabricManagerStatus NvFabricManagerStatus `json:"nv_fabric_manager_status"`
	XIDErrors             XIDErrors             `json:"xid_errors"`
	GpuDeviceCount        int                   `json:"gpu_device_count"`
	Gpus                  []GPUInfo             `json:"gpu_info"`
}

// NewGpuMetrics returns a zero-value GpuMetrics ready to be populated.
func NewGpuMetrics() *GpuMetrics {
	m := &GpuMetrics{}
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
		writer.Close()
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decompressJSON gzip-decompresses compressed and JSON-decodes the result into
// target.
func decompressJSON(compressed []byte, target interface{}) error {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return err
	}
	defer reader.Close()

	return json.NewDecoder(reader).Decode(target)
}

// ToBytes serializes the metrics to the on-the-wire representation: JSON
// followed by gzip compression. The returned bytes are the payload of a vsock
// frame (see pkg/vsock/common).
func (m *GpuMetrics) ToBytes() ([]byte, error) {
	return compressJSON(m)
}

// NewGpuMetricsFromBytes decodes a wire payload (gzip-compressed JSON) back
// into a GpuMetrics. It is the inverse of (*GpuMetrics).ToBytes.
func NewGpuMetricsFromBytes(data []byte) (*GpuMetrics, error) {
	m := NewGpuMetrics()
	err := decompressJSON(data, m)
	if err != nil {
		return nil, err
	}

	return m, nil
}
