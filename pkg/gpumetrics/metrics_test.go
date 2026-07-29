package gpumetrics

import (
	"testing"
)

func TestCompressDecompressRoundTrip(t *testing.T) {
	m := NewGpuMetrics()
	m.Source.InstanceID = "blabla"

	data, err := m.ToBytes()
	if err != nil {
		t.Fatal(err)
	}

	mr, err := NewGpuMetricsFromBytes(data)
	if err != nil {
		t.Fatal(err)
	}

	if m.Source.InstanceID != mr.Source.InstanceID {
		t.Fatal("unexpected result")
	}
}
