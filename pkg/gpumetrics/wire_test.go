package gpumetrics

import (
	"reflect"
	"testing"
)

// sampleGpuMetrics builds a fully-populated GpuMetrics covering every field
// family (incl. maps with int keys, deltas, nested slices) so a round-trip
// regression in any json tag surfaces as a diff.
func sampleGpuMetrics() *GpuMetrics {
	m := NewGpuMetrics()
	m.Source = GpuMetricsSource{VsockClientID: 42, InstanceID: "i-abc123"}
	m.ExporterInfo = ExporterInfo{
		Seqno: 7, Timestamp: 1700000000, Version: "2026.06.08-2", StartTime: 1699999000,
		InitNVMLError: "", GetDeviceCountError: "", ReadInstanceIDError: "",
		SendMetricsErrorCount: 3, SendMetricsLastError: "boom",
	}
	m.NvFabricManagerStatus = NvFabricManagerStatus{Active: true, Enabled: true, NotDegraded: true}
	m.XIDErrors = XIDErrors{XIDErrors: []string{"Xid 79 on GPU 0", "SXid 13"}, DroppedCount: 4}
	m.GpuDeviceCount = 2
	m.Gpus = []GPUInfo{
		{
			Index: 0, UUID: "GPU-aaaa", Name: "H100", Serial: "SN1",
			Temperature: 45, MemoryTemp: 50,
			PowerUsage: 350, PowerLimit: 700,
			MemoryTotal: 80 * 1024 * 1024 * 1024, MemoryUsed: 1024, MemoryFree: 1023,
			BAR1MemoryTotal: 256, BAR1MemoryUsed: 10, BAR1MemoryFree: 246,
			Utilization: GPUUtilization{GPU: 99, Memory: 50},
			DecoderUtil: 12, EncoderUtil: 34,
			Clocks:   GPUClocks{Graphics: 1095, Memory: 1593, SM: 1095},
			FanSpeed: 0,
			PCI: PCIInfo{
				BusID: "0000:01:00.0", Domain: 0, Bus: 1, Device: 0,
				PCIGen: 5, LinkWidth: 16, MaxPCIGen: 5, MaxLinkWidth: 16,
				TxThroughput: 1234, RxThroughput: 4321,
			},
			DriverModel:           "WDDM",
			DriverVersion:         "535.129.03",
			VBios:                 "96.00.7F.00.01",
			ComputeMode:           "Default",
			ComputeModeValue:      0,
			PerformanceState:      "P0",
			PerformanceStateValue: 0,
			ECC: ECCInfo{
				Enabled: true, Pending: false, Mode: "Enabled",
				DRAMErrors: ECCErrorsCounters{
					Volatile:  ECCErrors{Correctable: 10, Uncorrectable: 1, CorrectableDelta: 2, UncorrectableDelta: 1},
					Aggregate: ECCErrors{Correctable: 100, Uncorrectable: 5, CorrectableDelta: 0, UncorrectableDelta: 0},
				},
				SRAMErrors: ECCErrorsCounters{
					Volatile:  ECCErrors{Correctable: 3, Uncorrectable: 0, CorrectableDelta: 3, UncorrectableDelta: 0},
					Aggregate: ECCErrors{Correctable: 9, Uncorrectable: 0, CorrectableDelta: 0, UncorrectableDelta: 0},
				},
				RetiredPages: RetiredPagesInfo{
					SBEPages: 2, DBEPages: 0, SBEPagesDelta: 1, DBEPagesDelta: 0,
					PendingPages: 0, PendingPagesDelta: 0, Error: "",
				},
			},
			RowRemapping: RowRemappingInfo{
				Pending: false, Failed: false,
				Correctable: 4, Uncorrectable: 0, CorrectableDelta: 1, UncorrectableDelta: 0,
			},
			ClocksThrottle: ClocksThrottleInfo{
				ThrottleReasons: 0, ThrottleReasonsStr: "None",
				EventReasons: 0, EventReasonsStr: "None",
			},
			NvLink: NvLinkInfo{Links: []NvLinkState{
				{
					LinkIndex: 0, State: "Active",
					Errors:      map[int]uint64{0: 5, 1: 7},
					ErrorsDelta: map[int]int64{0: 1, 1: 2},
				},
				{LinkIndex: 1, State: "Inactive", Errors: map[int]uint64{}, ErrorsDelta: map[int]int64{}},
			}},
			Architecture:          "Hopper",
			CUDAComputeCapability: CUDAComputeCapability{Major: 9, Minor: 0},
			PersistenceMode:       1,
		},
		{Index: 1, UUID: "GPU-bbbb", Name: "H100", Error: ""},
	}
	return m
}

func TestToBytesFromBytesFullRoundTrip(t *testing.T) {
	original := sampleGpuMetrics()

	data, err := original.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("ToBytes returned empty data")
	}

	got, err := NewGpuMetricsFromBytes(data)
	if err != nil {
		t.Fatalf("NewGpuMetricsFromBytes: %v", err)
	}

	if !reflect.DeepEqual(original, got) {
		t.Fatalf("round-trip mismatch:\nwant: %#v\ngot:  %#v", original, got)
	}
}

func TestNewGpuMetricsFromBytesInvalidGzip(t *testing.T) {
	_, err := NewGpuMetricsFromBytes([]byte("this is definitely not gzip"))
	if err == nil {
		t.Fatal("expected error for non-gzip input, got nil")
	}
}

func TestNewGpuMetricsFromBytesTruncated(t *testing.T) {
	original := sampleGpuMetrics()
	data, err := original.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	// Cut into the compressed payload itself (not just the 8-byte gzip trailer,
	// whose loss leaves the JSON fully decodable). Halving the stream forces a
	// mid-block unexpected EOF during decode.
	truncated := data[:len(data)/2]
	if _, err := NewGpuMetricsFromBytes(truncated); err == nil {
		t.Fatal("expected error for truncated gzip, got nil")
	}
}

func TestNewGpuMetricsFromBytesEmpty(t *testing.T) {
	if _, err := NewGpuMetricsFromBytes(nil); err == nil {
		t.Fatal("expected error for nil input, got nil")
	}
	if _, err := NewGpuMetricsFromBytes([]byte{}); err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}

// TestEmptyStructRoundTrip ensures a zero-value GpuMetrics still round-trips
// (no nil-map / nil-slice panics on encode or decode).
func TestEmptyStructRoundTrip(t *testing.T) {
	original := NewGpuMetrics()

	data, err := original.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	got, err := NewGpuMetricsFromBytes(data)
	if err != nil {
		t.Fatalf("NewGpuMetricsFromBytes: %v", err)
	}

	// NewGpuMetricsFromBytes leaves Gpus as a nil slice; decode of an absent
	// json field also yields nil, so DeepEqual holds.
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("empty round-trip mismatch:\nwant: %#v\ngot:  %#v", original, got)
	}
}

func TestCompressJSONNonMarshalable(t *testing.T) {
	// compressJSON uses json.Marshal, which errors on unmarshallable values
	// like a channel. ToBytes must propagate that error, not panic.
	_, err := compressJSON(make(chan int))
	if err == nil {
		t.Fatal("expected error for non-marshalable input, got nil")
	}
}
