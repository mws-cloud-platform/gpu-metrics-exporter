package gpumetricsexporter

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

func newMIGTestExporter() *GpuMetricsExporter {
	return &GpuMetricsExporter{log: zap.NewNop()}
}

// migInstanceDevice builds a mock MIG device handle answering the subset of the
// device API a MIG handle really supports.
func migInstanceDevice(uuid, name string, gi, ci int, memTotal uint64, sms, giSlices, ciSlices uint32) *mock.Device {
	return &mock.Device{
		GetUUIDFunc:              func() (string, nvml.Return) { return uuid, nvml.SUCCESS },
		GetNameFunc:              func() (string, nvml.Return) { return name, nvml.SUCCESS },
		GetGpuInstanceIdFunc:     func() (int, nvml.Return) { return gi, nvml.SUCCESS },
		GetComputeInstanceIdFunc: func() (int, nvml.Return) { return ci, nvml.SUCCESS },
		GetMemoryInfoFunc: func() (nvml.Memory, nvml.Return) {
			return nvml.Memory{Total: memTotal, Used: memTotal / 4, Free: memTotal - memTotal/4}, nvml.SUCCESS
		},
		GetAttributesFunc: func() (nvml.DeviceAttributes, nvml.Return) {
			return nvml.DeviceAttributes{
				MultiprocessorCount:       sms,
				GpuInstanceSliceCount:     giSlices,
				ComputeInstanceSliceCount: ciSlices,
			}, nvml.SUCCESS
		},
	}
}

// TestCollectMIGInfoNotSupported covers the common case: a pre-Ampere card, or
// any GPU whose driver reports MIG unsupported. That must leave Supported false
// WITHOUT recording an error — it is normal hardware, not a collection failure.
func TestCollectMIGInfoNotSupported(t *testing.T) {
	e := newMIGTestExporter()
	device := &mock.Device{
		GetMigModeFunc: func() (int, int, nvml.Return) { return 0, 0, nvml.ERROR_NOT_SUPPORTED },
	}

	var info gpumetrics.GPUInfo
	e.collectMIGInfo(device, &info)

	if info.MIG.Supported {
		t.Error("Supported = true on a GPU where NVML reports MIG unsupported")
	}
	if info.MIG.Error != "" {
		t.Errorf("Error = %q, want empty: unsupported MIG is not a collection failure", info.MIG.Error)
	}
	if len(info.MIG.Instances) != 0 {
		t.Errorf("got %d instances, want 0", len(info.MIG.Instances))
	}
}

// TestCollectMIGInfoSupportedButDisabled covers a MIG-capable GPU running in
// full-card mode: supported, not enabled, and no instance walk.
func TestCollectMIGInfoSupportedButDisabled(t *testing.T) {
	e := newMIGTestExporter()
	walked := false
	device := &mock.Device{
		GetMigModeFunc: func() (int, int, nvml.Return) {
			return nvml.DEVICE_MIG_DISABLE, nvml.DEVICE_MIG_DISABLE, nvml.SUCCESS
		},
		GetMaxMigDeviceCountFunc: func() (int, nvml.Return) { walked = true; return 7, nvml.SUCCESS },
	}

	var info gpumetrics.GPUInfo
	e.collectMIGInfo(device, &info)

	if !info.MIG.Supported {
		t.Error("Supported = false on a GPU that answered GetMigMode")
	}
	if info.MIG.Enabled || info.MIG.PendingEnabled || info.MIG.PendingChange {
		t.Errorf("expected all-off state, got %+v", info.MIG)
	}
	if walked {
		t.Error("enumerated MIG devices with MIG disabled; every index would fail")
	}
}

// TestCollectMIGInfoPendingChange covers the operator-visible case that a plain
// enabled/disabled bit would hide: MIG has been switched on but has not taken
// effect, because it needs a GPU reset. The running partitioning is not the
// configured one, and pending_change is what says so.
func TestCollectMIGInfoPendingChange(t *testing.T) {
	e := newMIGTestExporter()
	device := &mock.Device{
		GetMigModeFunc: func() (int, int, nvml.Return) {
			return nvml.DEVICE_MIG_DISABLE, nvml.DEVICE_MIG_ENABLE, nvml.SUCCESS
		},
	}

	var info gpumetrics.GPUInfo
	e.collectMIGInfo(device, &info)

	if !info.MIG.Supported {
		t.Fatal("Supported = false")
	}
	if info.MIG.Enabled {
		t.Error("Enabled = true, but the current mode is DISABLE")
	}
	if !info.MIG.PendingEnabled {
		t.Error("PendingEnabled = false, but the pending mode is ENABLE")
	}
	if !info.MIG.PendingChange {
		t.Error("PendingChange = false, but current and pending modes differ")
	}
}

// TestCollectMIGInfoEnumeratesInstances is the main path: MIG on, with a
// partitioning that leaves gaps. NVML reports empty slots as ERROR_NOT_FOUND,
// which must be skipped silently rather than recorded as a failed instance —
// a 2x3g.40gb layout on a 7-slice A100 populates only some indices.
func TestCollectMIGInfoEnumeratesInstances(t *testing.T) {
	e := newMIGTestExporter()

	const gib = 1024 * 1024 * 1024
	first := migInstanceDevice("MIG-aaa", "NVIDIA A100-SXM4-40GB MIG 3g.20gb", 1, 0, 20*gib, 42, 3, 3)
	second := migInstanceDevice("MIG-bbb", "NVIDIA A100-SXM4-40GB MIG 3g.20gb", 5, 0, 20*gib, 42, 3, 3)

	device := &mock.Device{
		GetMigModeFunc: func() (int, int, nvml.Return) {
			return nvml.DEVICE_MIG_ENABLE, nvml.DEVICE_MIG_ENABLE, nvml.SUCCESS
		},
		GetMaxMigDeviceCountFunc: func() (int, nvml.Return) { return 7, nvml.SUCCESS },
		GetMigDeviceHandleByIndexFunc: func(n int) (nvml.Device, nvml.Return) {
			switch n {
			case 0:
				return first, nvml.SUCCESS
			case 3:
				return second, nvml.SUCCESS
			default:
				return nil, nvml.ERROR_NOT_FOUND // empty slot, not an error
			}
		},
	}

	var info gpumetrics.GPUInfo
	e.collectMIGInfo(device, &info)

	if !info.MIG.Enabled {
		t.Fatal("Enabled = false")
	}
	if info.MIG.Error != "" {
		t.Fatalf("Error = %q, want empty", info.MIG.Error)
	}
	if info.MIG.InstanceCount != 2 || len(info.MIG.Instances) != 2 {
		t.Fatalf("InstanceCount = %d, len(Instances) = %d, want 2 and 2 (empty slots must be skipped)",
			info.MIG.InstanceCount, len(info.MIG.Instances))
	}

	got := info.MIG.Instances[0]
	if got.Index != 0 || got.UUID != "MIG-aaa" || got.GpuInstanceID != 1 || got.ComputeInstanceID != 0 {
		t.Errorf("first instance = %+v, want index 0 / MIG-aaa / gi 1 / ci 0", got)
	}
	if got.MemoryTotal != 20*gib || got.MultiprocessorCount != 42 || got.GpuInstanceSliceCount != 3 {
		t.Errorf("first instance attributes = %+v", got)
	}
	// The second populated slot keeps its real NVML index, not its position in
	// the slice: downstream has to line it up with nvidia-smi output.
	if info.MIG.Instances[1].Index != 3 || info.MIG.Instances[1].UUID != "MIG-bbb" {
		t.Errorf("second instance = %+v, want index 3 / MIG-bbb", info.MIG.Instances[1])
	}
}

// TestCollectMIGInfoInstanceHandleError records a genuine per-slot failure as
// an instance carrying an Error, rather than dropping it: a slot that exists
// but cannot be read is a different condition from an empty slot.
func TestCollectMIGInfoInstanceHandleError(t *testing.T) {
	e := newMIGTestExporter()
	device := &mock.Device{
		GetMigModeFunc: func() (int, int, nvml.Return) {
			return nvml.DEVICE_MIG_ENABLE, nvml.DEVICE_MIG_ENABLE, nvml.SUCCESS
		},
		GetMaxMigDeviceCountFunc: func() (int, nvml.Return) { return 2, nvml.SUCCESS },
		GetMigDeviceHandleByIndexFunc: func(n int) (nvml.Device, nvml.Return) {
			if n == 0 {
				return nil, nvml.ERROR_UNKNOWN
			}
			return nil, nvml.ERROR_NOT_FOUND
		},
	}

	var info gpumetrics.GPUInfo
	e.collectMIGInfo(device, &info)

	if len(info.MIG.Instances) != 1 {
		t.Fatalf("got %d instances, want 1 (the failing slot)", len(info.MIG.Instances))
	}
	if info.MIG.Instances[0].Error == "" {
		t.Error("failing slot recorded with an empty Error")
	}
}

// TestCollectMIGInfoModeQueryFails records a real failure on a card that should
// have answered, distinguishing it from the unsupported case above.
func TestCollectMIGInfoModeQueryFails(t *testing.T) {
	e := newMIGTestExporter()
	device := &mock.Device{
		GetMigModeFunc: func() (int, int, nvml.Return) { return 0, 0, nvml.ERROR_UNKNOWN },
	}

	var info gpumetrics.GPUInfo
	e.collectMIGInfo(device, &info)

	if info.MIG.Supported {
		t.Error("Supported = true despite a failed GetMigMode")
	}
	if info.MIG.Error == "" {
		t.Error("Error is empty; a genuine GetMigMode failure must be reported")
	}
}
