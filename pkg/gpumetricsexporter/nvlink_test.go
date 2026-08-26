package gpumetricsexporter

import (
	"strings"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

func newNvLinkTestExporter() *GpuMetricsExporter {
	return &GpuMetricsExporter{log: zap.NewNop()}
}

// nvLinkDevice builds a mock GPU whose real links are exactly the keys of
// counters. Every other index answers NOT_SUPPORTED, which is how NVML reports
// a link the card does not have — NVLINK_MAX_LINKS is 18 whatever the board is.
// A counter missing from a link's map answers NOT_SUPPORTED too, the normal
// case for an inactive link.
func nvLinkDevice(state nvml.EnableState, counters map[int]map[nvml.NvLinkErrorCounter]uint64) *mock.Device {
	return &mock.Device{
		GetNvLinkStateFunc: func(link int) (nvml.EnableState, nvml.Return) {
			if _, ok := counters[link]; !ok {
				return 0, nvml.ERROR_NOT_SUPPORTED
			}
			return state, nvml.SUCCESS
		},
		GetNvLinkErrorCounterFunc: func(link int, counter nvml.NvLinkErrorCounter) (uint64, nvml.Return) {
			linkCounters, ok := counters[link]
			if !ok {
				return 0, nvml.ERROR_NOT_SUPPORTED
			}
			value, ok := linkCounters[counter]
			if !ok {
				return 0, nvml.ERROR_NOT_SUPPORTED
			}
			return value, nvml.SUCCESS
		},
	}
}

// activeLinks builds counter maps for n links, each reporting DL_REPLAY only.
func activeLinks(n int, base uint64) map[int]map[nvml.NvLinkErrorCounter]uint64 {
	links := make(map[int]map[nvml.NvLinkErrorCounter]uint64, n)
	for i := 0; i < n; i++ {
		links[i] = map[nvml.NvLinkErrorCounter]uint64{nvml.NVLINK_ERROR_DL_REPLAY: base + uint64(i)}
	}
	return links
}

// TestCollectNvLinkInfoSkipsLinksTheGPUDoesNotHave pins the A100 shape: 12 real
// links out of a possible 18. The other six must not reach the payload at all —
// they used to arrive as entries carrying nothing but "GetNvLinkState error:
// Not Supported", on every GPU, every tick.
func TestCollectNvLinkInfoSkipsLinksTheGPUDoesNotHave(t *testing.T) {
	e := newNvLinkTestExporter()
	device := nvLinkDevice(nvml.NVLINK_STATE_ACTIVE, activeLinks(12, 100))

	var info gpumetrics.GPUInfo
	e.collectNvLinkInfo(device, "GPU-a100", &info)

	if !info.NvLink.Supported {
		t.Error("Supported = false on a GPU reporting 12 links")
	}
	if got := len(info.NvLink.Links); got != 12 {
		t.Fatalf("got %d links, want 12 (18 is the header ceiling, not the count)", got)
	}
	for i, link := range info.NvLink.Links {
		if link.LinkIndex != i {
			t.Errorf("Links[%d].LinkIndex = %d, want %d", i, link.LinkIndex, i)
		}
		if link.Error != "" {
			t.Errorf("Links[%d].Error = %q, want empty", i, link.Error)
		}
		if link.State != "Active" {
			t.Errorf("Links[%d].State = %q, want Active", i, link.State)
		}
		if got := link.Errors[int(nvml.NVLINK_ERROR_DL_REPLAY)]; got != uint64(100+i) {
			t.Errorf("Links[%d] DL_REPLAY = %d, want %d", i, got, 100+i)
		}
	}
}

// TestCollectNvLinkInfoNoLinksAtAll covers a card with no NVLink. Supported is
// what tells that apart from links that could not be enumerated, now that an
// absent link produces no entry to look at.
func TestCollectNvLinkInfoNoLinksAtAll(t *testing.T) {
	e := newNvLinkTestExporter()
	device := nvLinkDevice(nvml.NVLINK_STATE_ACTIVE, nil)

	var info gpumetrics.GPUInfo
	e.collectNvLinkInfo(device, "GPU-nolink", &info)

	if info.NvLink.Supported {
		t.Error("Supported = true on a GPU reporting no links")
	}
	if got := len(info.NvLink.Links); got != 0 {
		t.Errorf("got %d links, want 0", got)
	}
}

// TestCollectNvLinkInfoInactiveLinkIsNotAnError is the case from the field: the
// links exist, they are idle, and NVML declines every error counter. An empty
// Errors map already says the counters are unavailable — an error string per
// counter per link per tick says nothing extra and drowns real failures.
func TestCollectNvLinkInfoInactiveLinkIsNotAnError(t *testing.T) {
	e := newNvLinkTestExporter()
	// Links present but no counters served: exactly what an A100 with unused
	// NVLink bridges reports.
	device := nvLinkDevice(nvml.NVLINK_STATE_INACTIVE, map[int]map[nvml.NvLinkErrorCounter]uint64{
		0: {}, 1: {},
	})

	var info gpumetrics.GPUInfo
	e.collectNvLinkInfo(device, "GPU-idle", &info)

	if got := len(info.NvLink.Links); got != 2 {
		t.Fatalf("got %d links, want 2", got)
	}
	for i, link := range info.NvLink.Links {
		if link.State != "Inactive" {
			t.Errorf("Links[%d].State = %q, want Inactive", i, link.State)
		}
		if link.Error != "" {
			t.Errorf("Links[%d].Error = %q, want empty: an unsupported counter is not a failure", i, link.Error)
		}
		if len(link.Errors) != 0 {
			t.Errorf("Links[%d].Errors = %v, want empty", i, link.Errors)
		}
	}
}

// TestCollectNvLinkInfoKeepsGenuineStateFailure: a link that exists but cannot
// be read is the signal the error field is for, so its entry must survive the
// skipping that removes non-existent links.
func TestCollectNvLinkInfoKeepsGenuineStateFailure(t *testing.T) {
	e := newNvLinkTestExporter()
	device := &mock.Device{
		GetNvLinkStateFunc: func(link int) (nvml.EnableState, nvml.Return) {
			if link == 3 {
				return 0, nvml.ERROR_UNKNOWN
			}
			return 0, nvml.ERROR_NOT_SUPPORTED
		},
	}

	var info gpumetrics.GPUInfo
	e.collectNvLinkInfo(device, "GPU-broken", &info)

	if got := len(info.NvLink.Links); got != 1 {
		t.Fatalf("got %d links, want 1 (only the one that failed for a real reason)", got)
	}
	link := info.NvLink.Links[0]
	if link.LinkIndex != 3 {
		t.Errorf("LinkIndex = %d, want 3", link.LinkIndex)
	}
	if !strings.Contains(link.Error, "GetNvLinkState") {
		t.Errorf("Error = %q, want a GetNvLinkState failure", link.Error)
	}
	if info.NvLink.Supported {
		t.Error("Supported = true although no link was successfully enumerated")
	}
}

// TestCollectNvLinkInfoReportsFirstFailingCounter guards the last-writer-wins
// trap: five counters share one link, and letting each overwrite the previous
// message meant a link whose counters had all failed reported only "counter 4".
func TestCollectNvLinkInfoReportsFirstFailingCounter(t *testing.T) {
	e := newNvLinkTestExporter()
	device := &mock.Device{
		GetNvLinkStateFunc: func(int) (nvml.EnableState, nvml.Return) {
			return nvml.NVLINK_STATE_ACTIVE, nvml.SUCCESS
		},
		GetNvLinkErrorCounterFunc: func(_ int, counter nvml.NvLinkErrorCounter) (uint64, nvml.Return) {
			return 0, nvml.ERROR_UNKNOWN
		},
	}

	var info gpumetrics.GPUInfo
	e.collectNvLinkInfo(device, "GPU-counters", &info)

	if len(info.NvLink.Links) == 0 {
		t.Fatal("no links collected")
	}
	if got := info.NvLink.Links[0].Error; !strings.Contains(got, "GetNvLinkErrorCounter 0 ") {
		t.Errorf("Error = %q, want the first failing counter (0), not the last", got)
	}
}

// TestCollectNvLinkInfoDeltaMatchesByLinkIndex is the regression test for the
// lookup that skipping links would otherwise break. The previous tick's slice
// holds links 2..4 at positions 0..2, so position and link index disagree;
// diffing by position would read link 2 against link 4's counters and lose the
// rest off the end of the slice.
func TestCollectNvLinkInfoDeltaMatchesByLinkIndex(t *testing.T) {
	e := newNvLinkTestExporter()
	e.lastMetrics = &gpumetrics.GpuMetrics{
		Gpus: []gpumetrics.GPUInfo{{
			UUID: "GPU-sparse",
			NvLink: gpumetrics.NvLinkInfo{Supported: true, Links: []gpumetrics.NvLinkState{
				{LinkIndex: 2, Errors: map[int]uint64{int(nvml.NVLINK_ERROR_DL_REPLAY): 10}},
				{LinkIndex: 3, Errors: map[int]uint64{int(nvml.NVLINK_ERROR_DL_REPLAY): 20}},
				{LinkIndex: 4, Errors: map[int]uint64{int(nvml.NVLINK_ERROR_DL_REPLAY): 30}},
			}},
		}},
	}

	device := nvLinkDevice(nvml.NVLINK_STATE_ACTIVE, map[int]map[nvml.NvLinkErrorCounter]uint64{
		2: {nvml.NVLINK_ERROR_DL_REPLAY: 15},
		3: {nvml.NVLINK_ERROR_DL_REPLAY: 25},
		4: {nvml.NVLINK_ERROR_DL_REPLAY: 35},
	})

	var info gpumetrics.GPUInfo
	e.collectNvLinkInfo(device, "GPU-sparse", &info)

	if got := len(info.NvLink.Links); got != 3 {
		t.Fatalf("got %d links, want 3", got)
	}
	for _, link := range info.NvLink.Links {
		got, ok := link.ErrorsDelta[int(nvml.NVLINK_ERROR_DL_REPLAY)]
		if !ok {
			t.Errorf("link %d: no DL_REPLAY delta; the previous tick's entry was not found", link.LinkIndex)
			continue
		}
		if got != 5 {
			t.Errorf("link %d: DL_REPLAY delta = %d, want 5 (diffed against a different link)", link.LinkIndex, got)
		}
	}
}
