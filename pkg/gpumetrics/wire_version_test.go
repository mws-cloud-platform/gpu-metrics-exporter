package gpumetrics

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	v1 "go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics/v1"
)

// fillNonZero recursively sets every leaf in v to a distinct non-zero value.
// Populating the v1 sample by reflection rather than by hand is deliberate: a
// hand-written sample can only test the fields someone remembered to write,
// which is the same failure mode the upconverter itself has.
func fillNonZero(v reflect.Value, seed *int) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillNonZero(v.Field(i), seed)
		}
	case reflect.String:
		*seed++
		v.SetString(fmt.Sprintf("str-%d", *seed))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*seed++
		v.SetInt(int64(*seed))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		*seed++
		v.SetUint(uint64(*seed))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(elem, seed)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillNonZero(key, seed)
		val := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(val, seed)
		m.SetMapIndex(key, val)
		v.Set(m)
	}
}

// v1ToV2KeyRenames maps json paths that changed spelling between v1 and v2.
// Anything not listed must keep its path and value through the upconversion.
var v1ToV2KeyRenames = map[string]string{
	"gpu_info[].pci_info.tx_throughtput": "gpu_info[].pci_info.tx_throughput",
	"gpu_info[].pci_info.rx_throughtput": "gpu_info[].pci_info.rx_throughput",
}

// flatten walks decoded JSON into path -> scalar pairs. Slice indices collapse
// to "[]" so paths are position-independent.
func flatten(prefix string, node any, out map[string]any) {
	switch n := node.(type) {
	case map[string]any:
		for k, val := range n {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flatten(p, val, out)
		}
	case []any:
		for _, val := range n {
			flatten(prefix+"[]", val, out)
		}
	default:
		out[prefix] = node
	}
}

func flattenJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]any{}
	flatten("", decoded, out)
	return out
}

// TestUpconvertV1LosesNothing is the guard that makes the upconverter
// trustworthy. Every field a v1 exporter can populate must survive into the
// current shape — a forgotten field would silently read as zero on the host,
// which is indistinguishable from a real zero and therefore invisible.
func TestUpconvertV1LosesNothing(t *testing.T) {
	var m1 v1.GpuMetrics
	seed := 0
	fillNonZero(reflect.ValueOf(&m1).Elem(), &seed)
	t.Logf("populated v1 sample with %d distinct values", seed)

	got := upconvertV1(&m1)

	v1Flat := flattenJSON(t, &m1)
	v2Flat := flattenJSON(t, got)

	if len(v1Flat) == 0 {
		t.Fatal("v1 sample flattened to nothing; the filler is broken")
	}

	for path, want := range v1Flat {
		target := path
		if renamed, ok := v1ToV2KeyRenames[path]; ok {
			target = renamed
			if _, stillThere := v2Flat[path]; stillThere {
				t.Errorf("v1 path %q survived into v2 output; it should appear only as %q", path, target)
			}
		}

		have, ok := v2Flat[target]
		if !ok {
			t.Errorf("v1 field %q has no counterpart at %q after upconversion — data dropped", path, target)
			continue
		}
		if !reflect.DeepEqual(have, want) {
			t.Errorf("value mismatch at %q: v1 had %v, upconverted has %v", path, want, have)
		}
	}
}

// TestUpconvertV1MarksProvenance covers the reason WireVersion is carried
// rather than normalised away: fields v1 could not report must be
// distinguishable from fields it reported as zero.
func TestUpconvertV1MarksProvenance(t *testing.T) {
	var m1 v1.GpuMetrics
	seed := 0
	fillNonZero(reflect.ValueOf(&m1).Elem(), &seed)

	got := upconvertV1(&m1)

	if got.WireVersion != 1 {
		t.Errorf("WireVersion = %d, want 1: the payload came from a v1 exporter", got.WireVersion)
	}
	for i, gpu := range got.Gpus {
		if gpu.MIG.Supported || gpu.MIG.Enabled || len(gpu.MIG.Instances) != 0 {
			t.Errorf("gpu %d: MIG populated from a v1 payload (%+v); v1 never collected it", i, gpu.MIG)
		}
	}
}

// TestWireVersionDispatch covers the version routing itself, including the two
// boundary cases: a pre-versioning payload with no wire_version field at all,
// and a payload from an exporter newer than this receiver.
func TestWireVersionDispatch(t *testing.T) {
	t.Run("absent wire_version decodes as v1", func(t *testing.T) {
		var m1 v1.GpuMetrics
		m1.GpuDeviceCount = 3
		m1.Gpus = []v1.GPUInfo{{Index: 0, PCI: v1.PCIInfo{TxThroughput: 111, RxThroughput: 222}}}

		raw, err := json.Marshal(&m1)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if bytesContain(raw, "wire_version") {
			t.Fatal("v1 sample must not carry a wire_version field")
		}

		got := decodeGzipped(t, raw)
		if got.WireVersion != 1 {
			t.Errorf("WireVersion = %d, want 1", got.WireVersion)
		}
		// The rename must be resolved, not silently zeroed.
		if got.Gpus[0].PCI.TxThroughput != 111 || got.Gpus[0].PCI.RxThroughput != 222 {
			t.Errorf("throughput = %d/%d, want 111/222: the v1 key spelling was not mapped",
				got.Gpus[0].PCI.TxThroughput, got.Gpus[0].PCI.RxThroughput)
		}
	})

	t.Run("current version round-trips", func(t *testing.T) {
		m := sampleGpuMetrics()
		data, err := m.ToBytes()
		if err != nil {
			t.Fatalf("ToBytes: %v", err)
		}
		got, err := NewGpuMetricsFromBytes(data)
		if err != nil {
			t.Fatalf("NewGpuMetricsFromBytes: %v", err)
		}
		if got.WireVersion != CurrentWireVersion {
			t.Errorf("WireVersion = %d, want %d", got.WireVersion, CurrentWireVersion)
		}
		if !reflect.DeepEqual(m, got) {
			t.Error("current-version round-trip changed the payload")
		}
	})

	t.Run("future version decodes optimistically", func(t *testing.T) {
		m := sampleGpuMetrics()
		m.WireVersion = CurrentWireVersion + 5
		data, err := m.ToBytes()
		if err != nil {
			t.Fatalf("ToBytes: %v", err)
		}
		got, err := NewGpuMetricsFromBytes(data)
		if err != nil {
			t.Fatalf("a future version must not be a hard decode failure: %v", err)
		}
		if got.WireVersion != CurrentWireVersion+5 {
			t.Errorf("WireVersion = %d, want the value the exporter sent (%d) preserved",
				got.WireVersion, CurrentWireVersion+5)
		}
		if got.GpuDeviceCount != m.GpuDeviceCount {
			t.Error("known fields should still decode from a future payload")
		}
	})
}

func bytesContain(b []byte, sub string) bool {
	return len(b) > 0 && json.Valid(b) && containsString(string(b), sub)
}

func containsString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func decodeGzipped(t *testing.T, rawJSON []byte) *GpuMetrics {
	t.Helper()
	gzipped, err := compressRaw(rawJSON)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	got, err := NewGpuMetricsFromBytes(gzipped)
	if err != nil {
		t.Fatalf("NewGpuMetricsFromBytes: %v", err)
	}
	return got
}
