package gpumetricsexporter

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

func newECCTestExporter() *GpuMetricsExporter {
	return &GpuMetricsExporter{log: zap.NewNop()}
}

// TestCollectECCInfoNotSupported: a part without ECC is not a collection
// failure, so it leaves Supported false and Error empty.
func TestCollectECCInfoNotSupported(t *testing.T) {
	e := newECCTestExporter()
	device := &mock.Device{
		GetEccModeFunc: func() (nvml.EnableState, nvml.EnableState, nvml.Return) {
			return 0, 0, nvml.ERROR_NOT_SUPPORTED
		},
	}

	var info gpumetrics.GPUInfo
	e.collectECCInfo(device, "GPU-noecc", &info)

	if info.ECC.Supported {
		t.Error("Supported = true where NVML reports no ECC")
	}
	if info.ECC.Error != "" {
		t.Errorf("Error = %q, want empty", info.ECC.Error)
	}
}

// TestCollectECCInfoQueryFailureIsReported guards the gap this split closed: a
// broken GetEccMode used to return silently, leaving a payload that read
// exactly like a healthy GPU with ECC switched off.
func TestCollectECCInfoQueryFailureIsReported(t *testing.T) {
	e := newECCTestExporter()
	device := &mock.Device{
		GetEccModeFunc: func() (nvml.EnableState, nvml.EnableState, nvml.Return) {
			return 0, 0, nvml.ERROR_UNKNOWN
		},
	}

	var info gpumetrics.GPUInfo
	e.collectECCInfo(device, "GPU-broken", &info)

	if info.ECC.Supported {
		t.Error("Supported = true although the query failed")
	}
	if !strings.Contains(info.ECC.Error, "GetEccMode") {
		t.Errorf("Error = %q, want a GetEccMode failure", info.ECC.Error)
	}
}

// TestCollectECCInfoDisabled: ECC present but switched off. Supported records
// that the query answered, which is what separates this from the case above.
func TestCollectECCInfoDisabled(t *testing.T) {
	e := newECCTestExporter()
	device := &mock.Device{
		GetEccModeFunc: func() (nvml.EnableState, nvml.EnableState, nvml.Return) {
			return 0, 1, nvml.SUCCESS
		},
	}

	var info gpumetrics.GPUInfo
	e.collectECCInfo(device, "GPU-eccoff", &info)

	if !info.ECC.Supported {
		t.Error("Supported = false although GetEccMode answered")
	}
	if info.ECC.Enabled {
		t.Error("Enabled = true for current mode 0")
	}
	if !info.ECC.Pending {
		t.Error("Pending = false for pending mode 1")
	}
	if info.ECC.Mode != "Disabled" {
		t.Errorf("Mode = %q, want Disabled", info.ECC.Mode)
	}
	if info.ECC.Error != "" {
		t.Errorf("Error = %q, want empty", info.ECC.Error)
	}
}

// fieldValueDevice answers GetFieldValues with a per-field return code and
// value, the way NVML does: the batch succeeds while individual fields may be
// unsupported.
func fieldValueDevice(batch nvml.Return, perField map[uint32]struct {
	ret   nvml.Return
	value uint64
}) *mock.Device {
	return &mock.Device{
		GetFieldValuesFunc: func(values []nvml.FieldValue) nvml.Return {
			if batch != nvml.SUCCESS {
				return batch
			}
			for i := range values {
				f, ok := perField[values[i].FieldId]
				if !ok {
					values[i].NvmlReturn = uint32(nvml.ERROR_NOT_SUPPORTED)
					continue
				}
				values[i].NvmlReturn = uint32(f.ret)
				binary.LittleEndian.PutUint64(values[i].Value[:], f.value)
			}
			return nvml.SUCCESS
		},
	}
}

type fieldAnswer = struct {
	ret   nvml.Return
	value uint64
}

// TestCollectRetiredPagesNoFieldsSupported is the modern-fleet case: Ampere and
// later replaced page retirement with row remapping, so the batch succeeds
// while every field in it is unsupported. That is hardware, not a fault.
func TestCollectRetiredPagesNoFieldsSupported(t *testing.T) {
	e := newECCTestExporter()
	device := fieldValueDevice(nvml.SUCCESS, nil)

	var ecc gpumetrics.ECCInfo
	e.collectRetiredPages(device, "GPU-ampere", &ecc)

	if ecc.RetiredPages.Supported {
		t.Error("Supported = true although NVML served no retired-page field")
	}
	if ecc.RetiredPages.Error != "" {
		t.Errorf("Error = %q, want empty", ecc.RetiredPages.Error)
	}
	if ecc.RetiredPages.SBEPages != 0 || ecc.RetiredPages.DBEPages != 0 || ecc.RetiredPages.PendingPages != 0 {
		t.Errorf("counts = %+v, want all zero", ecc.RetiredPages)
	}
}

// TestCollectRetiredPagesPartialSupport: one served field is enough to make the
// block meaningful, and the unsupported ones must stay zero rather than pick up
// whatever was in the uninitialised Value bytes.
func TestCollectRetiredPagesPartialSupport(t *testing.T) {
	e := newECCTestExporter()
	device := fieldValueDevice(nvml.SUCCESS, map[uint32]fieldAnswer{
		nvml.FI_DEV_RETIRED_SBE: {nvml.SUCCESS, 7},
	})

	var ecc gpumetrics.ECCInfo
	e.collectRetiredPages(device, "GPU-partial", &ecc)

	if !ecc.RetiredPages.Supported {
		t.Error("Supported = false although one field was served")
	}
	if ecc.RetiredPages.SBEPages != 7 {
		t.Errorf("SBEPages = %d, want 7", ecc.RetiredPages.SBEPages)
	}
	if ecc.RetiredPages.DBEPages != 0 || ecc.RetiredPages.PendingPages != 0 {
		t.Errorf("unsupported fields = %d/%d, want 0", ecc.RetiredPages.DBEPages, ecc.RetiredPages.PendingPages)
	}
	if ecc.RetiredPages.Error != "" {
		t.Errorf("Error = %q, want empty", ecc.RetiredPages.Error)
	}
}

// TestCollectRetiredPagesBatchOutcomes separates the two ways the batch call
// itself can decline: NOT_SUPPORTED is hardware, anything else is a fault.
func TestCollectRetiredPagesBatchOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		batch     nvml.Return
		wantError bool
	}{
		{name: "not supported is not an error", batch: nvml.ERROR_NOT_SUPPORTED, wantError: false},
		{name: "genuine failure is reported", batch: nvml.ERROR_UNKNOWN, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newECCTestExporter()
			var ecc gpumetrics.ECCInfo
			e.collectRetiredPages(fieldValueDevice(tc.batch, nil), "GPU-x", &ecc)

			if ecc.RetiredPages.Supported {
				t.Error("Supported = true although no value was read")
			}
			if got := ecc.RetiredPages.Error != ""; got != tc.wantError {
				t.Errorf("Error = %q, wantError = %v", ecc.RetiredPages.Error, tc.wantError)
			}
		})
	}
}

// eccCounterDevice answers GetMemoryErrorCounter per (counter scope, location).
func eccCounterDevice(answer func(nvml.EccCounterType, nvml.MemoryLocation) (uint64, nvml.Return)) *mock.Device {
	return &mock.Device{
		GetMemoryErrorCounterFunc: func(_ nvml.MemoryErrorType, counter nvml.EccCounterType, location nvml.MemoryLocation) (uint64, nvml.Return) {
			return answer(counter, location)
		},
	}
}

// TestCollectECCErrorsVolatileUnsupported is the MIG case: NVML declines the
// whole volatile scope while still serving the aggregate one. That is why the
// flag sits on the scope and not on the GPU.
func TestCollectECCErrorsVolatileUnsupported(t *testing.T) {
	e := newECCTestExporter()
	device := eccCounterDevice(func(counter nvml.EccCounterType, _ nvml.MemoryLocation) (uint64, nvml.Return) {
		if counter == nvml.VOLATILE_ECC {
			return 0, nvml.ERROR_NOT_SUPPORTED
		}
		return 42, nvml.SUCCESS
	})

	var counters gpumetrics.ECCErrorsCounters
	e.collectECCErrorsInternal(device, nvml.MEMORY_LOCATION_DEVICE_MEMORY, &counters)

	if counters.Volatile.Supported {
		t.Error("Volatile.Supported = true although NVML declined the scope")
	}
	if counters.Volatile.Error != "" {
		t.Errorf("Volatile.Error = %q, want empty", counters.Volatile.Error)
	}
	if !counters.Aggregate.Supported {
		t.Error("Aggregate.Supported = false although the scope was served")
	}
	if counters.Aggregate.Correctable != 42 || counters.Aggregate.Uncorrectable != 42 {
		t.Errorf("aggregate counts = %d/%d, want 42/42", counters.Aggregate.Correctable, counters.Aggregate.Uncorrectable)
	}
}

// TestCollectECCErrorsFirstFailureWins: the two queries in a scope used to
// overwrite each other's message, so only the last was ever visible.
func TestCollectECCErrorsFirstFailureWins(t *testing.T) {
	e := newECCTestExporter()
	device := eccCounterDevice(func(nvml.EccCounterType, nvml.MemoryLocation) (uint64, nvml.Return) {
		return 0, nvml.ERROR_UNKNOWN
	})

	var counters gpumetrics.ECCErrorsCounters
	e.collectECCErrorsInternal(device, nvml.MEMORY_LOCATION_DEVICE_MEMORY, &counters)

	if !strings.Contains(counters.Volatile.Error, "volatile ECC correctable") {
		t.Errorf("Volatile.Error = %q, want the first failing query", counters.Volatile.Error)
	}
	if !strings.Contains(counters.Aggregate.Error, "aggregate ECC correctable") {
		t.Errorf("Aggregate.Error = %q, want the first failing query", counters.Aggregate.Error)
	}
	if counters.Volatile.Supported || counters.Aggregate.Supported {
		t.Error("Supported = true although every query failed")
	}
}

// TestCollectRowRemappingInfo covers the mirror of retired pages: pre-Ampere
// cards decline it, and only a different failure is worth an error.
func TestCollectRowRemappingInfo(t *testing.T) {
	tests := []struct {
		name          string
		ret           nvml.Return
		wantSupported bool
		wantError     bool
	}{
		{name: "pre-Ampere declines", ret: nvml.ERROR_NOT_SUPPORTED, wantSupported: false, wantError: false},
		{name: "genuine failure", ret: nvml.ERROR_UNKNOWN, wantSupported: false, wantError: true},
		{name: "served", ret: nvml.SUCCESS, wantSupported: true, wantError: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newECCTestExporter()
			device := &mock.Device{
				GetRemappedRowsFunc: func() (int, int, bool, bool, nvml.Return) {
					return 4, 1, true, false, tc.ret
				},
			}

			var info gpumetrics.GPUInfo
			e.collectRowRemappingInfo(device, "GPU-rr", &info)

			if info.RowRemapping.Supported != tc.wantSupported {
				t.Errorf("Supported = %v, want %v", info.RowRemapping.Supported, tc.wantSupported)
			}
			if got := info.RowRemapping.Error != ""; got != tc.wantError {
				t.Errorf("Error = %q, wantError = %v", info.RowRemapping.Error, tc.wantError)
			}
			if tc.wantSupported {
				if info.RowRemapping.Correctable != 4 || info.RowRemapping.Uncorrectable != 1 {
					t.Errorf("counts = %d/%d, want 4/1", info.RowRemapping.Correctable, info.RowRemapping.Uncorrectable)
				}
				if !info.RowRemapping.Pending {
					t.Error("Pending = false, want true")
				}
			} else if info.RowRemapping.Correctable != 0 {
				t.Errorf("Correctable = %d on an unread query, want 0", info.RowRemapping.Correctable)
			}
		})
	}
}
