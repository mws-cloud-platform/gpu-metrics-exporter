package gpumetrics

import (
	"os"
	"testing"
)

// TestGoldenV1Payload decodes a captured v1 payload exactly as the receiver
// would. The fixture is stored as readable JSON rather than a gzip blob so a
// reviewer can see what is being asserted, and so a future change to the
// upconverter shows up as a diff in behaviour rather than in an opaque binary.
//
// This is the regression that matters most for versioning: the bytes in
// testdata are the shape that exporters already deployed in customer VMs
// actually send, and those VMs may never be upgraded.
func TestGoldenV1Payload(t *testing.T) {
	rawJSON, err := os.ReadFile("testdata/v1_payload.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	payload, err := compressRaw(rawJSON)
	if err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}

	m, err := NewGpuMetricsFromBytes(payload)
	if err != nil {
		t.Fatalf("NewGpuMetricsFromBytes: %v", err)
	}

	if m.WireVersion != 1 {
		t.Errorf("WireVersion = %d, want 1 (fixture carries no wire_version field)", m.WireVersion)
	}
	if m.Source.InstanceID != "i-legacy-guest" {
		t.Errorf("InstanceID = %q", m.Source.InstanceID)
	}
	if m.ExporterInfo.Seqno != 4211 || m.ExporterInfo.Version != "2026.07.29-1" {
		t.Errorf("ExporterInfo = %+v", m.ExporterInfo)
	}
	if got, want := len(m.XIDErrors.XIDErrors), 1; got != want {
		t.Fatalf("XIDErrors count = %d, want %d", got, want)
	}
	if m.XIDErrors.DroppedCount != 0 {
		t.Errorf("DroppedCount = %d, want 0: v1 had no overflow buffer to lose lines to", m.XIDErrors.DroppedCount)
	}

	if len(m.Gpus) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(m.Gpus))
	}
	gpu := m.Gpus[0]

	// The whole point of v1 support: these arrived under the misspelled keys.
	if gpu.PCI.TxThroughput != 9876 || gpu.PCI.RxThroughput != 5432 {
		t.Errorf("throughput = %d/%d, want 9876/5432 — tx_throughtput/rx_throughtput were not mapped",
			gpu.PCI.TxThroughput, gpu.PCI.RxThroughput)
	}

	// Spot-check that nesting survived, not just top-level scalars.
	if gpu.ECC.RetiredPages.DBEPages != 1 || gpu.ECC.RetiredPages.DBEPagesDelta != 1 {
		t.Errorf("retired pages = %+v", gpu.ECC.RetiredPages)
	}
	if gpu.ECC.DRAMErrors.Aggregate.Correctable != 480 {
		t.Errorf("aggregate DRAM correctable = %d, want 480", gpu.ECC.DRAMErrors.Aggregate.Correctable)
	}
	if len(gpu.NvLink.Links) != 1 || gpu.NvLink.Links[0].Errors[0] != 3 {
		t.Errorf("nvlink = %+v", gpu.NvLink)
	}
	if gpu.CUDAComputeCapability.Major != 8 || gpu.Architecture != "Ampere" {
		t.Errorf("arch = %q, cc = %+v", gpu.Architecture, gpu.CUDAComputeCapability)
	}

	// v2-only territory: absent because the exporter predates it, not because
	// the GPU lacks MIG. WireVersion above is what distinguishes the two.
	if gpu.MIG.Supported || gpu.MIG.Enabled || len(gpu.MIG.Instances) != 0 {
		t.Errorf("MIG populated from a v1 payload: %+v", gpu.MIG)
	}
}
