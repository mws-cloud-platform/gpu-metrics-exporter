package gpumetricsexporter

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"
)

// newXIDTestExporter builds a bare exporter with only the fields the XID
// buffer helpers touch. The helpers are pure state manipulation, so nothing
// else (NVML, ticker, queue) needs to exist.
func newXIDTestExporter() *GpuMetricsExporter {
	return &GpuMetricsExporter{log: zap.NewNop()}
}

// TestTrimUnsentXIDErrorsUnderCap leaves a buffer that fits alone: nothing is
// dropped and the drop counter stays at zero.
func TestTrimUnsentXIDErrorsUnderCap(t *testing.T) {
	e := newXIDTestExporter()
	e.unsentXIDErrors = []string{"Xid 79", "SXid 13"}

	e.trimUnsentXIDErrors()

	if len(e.unsentXIDErrors) != 2 {
		t.Fatalf("buffer len = %d, want 2", len(e.unsentXIDErrors))
	}
	if e.xidErrorsDropped != 0 {
		t.Fatalf("xidErrorsDropped = %d, want 0", e.xidErrorsDropped)
	}
}

// TestTrimUnsentXIDErrorsDropsOldest is the guard against the wedge: an
// unbounded buffer eventually pushes the gzipped payload past the 64 KiB vsock
// frame limit, after which SendData fails on size forever. The cap must hold,
// the survivors must be the newest lines, and the loss must be counted.
func TestTrimUnsentXIDErrorsDropsOldest(t *testing.T) {
	e := newXIDTestExporter()

	const overflow = 5
	for i := 0; i < maxUnsentXIDErrors+overflow; i++ {
		e.unsentXIDErrors = append(e.unsentXIDErrors, fmt.Sprintf("Xid line %d", i))
	}

	e.trimUnsentXIDErrors()

	if len(e.unsentXIDErrors) != maxUnsentXIDErrors {
		t.Fatalf("buffer len = %d, want %d", len(e.unsentXIDErrors), maxUnsentXIDErrors)
	}
	if e.xidErrorsDropped != overflow {
		t.Fatalf("xidErrorsDropped = %d, want %d", e.xidErrorsDropped, overflow)
	}
	// Oldest-first eviction: line `overflow` is now the head, the last line
	// appended is still the tail.
	if got, want := e.unsentXIDErrors[0], fmt.Sprintf("Xid line %d", overflow); got != want {
		t.Fatalf("head = %q, want %q (oldest lines should be dropped first)", got, want)
	}
	last := fmt.Sprintf("Xid line %d", maxUnsentXIDErrors+overflow-1)
	if got := e.unsentXIDErrors[len(e.unsentXIDErrors)-1]; got != last {
		t.Fatalf("tail = %q, want %q (newest line must survive)", got, last)
	}
}

// TestTrimUnsentXIDErrorsAccumulatesDropCount checks the counter is cumulative
// across ticks, since it is reported on the wire as a running total.
func TestTrimUnsentXIDErrorsAccumulatesDropCount(t *testing.T) {
	e := newXIDTestExporter()

	for round := 0; round < 3; round++ {
		for i := 0; i < maxUnsentXIDErrors+2; i++ {
			e.unsentXIDErrors = append(e.unsentXIDErrors, fmt.Sprintf("r%d line %d", round, i))
		}
		e.trimUnsentXIDErrors()
	}

	// Round 0 overflows by 2; rounds 1 and 2 each start from a full buffer and
	// add maxUnsentXIDErrors+2 more, so each drops that many.
	want := int64(2 + 2*(maxUnsentXIDErrors+2))
	if e.xidErrorsDropped != want {
		t.Fatalf("xidErrorsDropped = %d, want %d", e.xidErrorsDropped, want)
	}
	if len(e.unsentXIDErrors) != maxUnsentXIDErrors {
		t.Fatalf("buffer len = %d, want %d", len(e.unsentXIDErrors), maxUnsentXIDErrors)
	}
}

// TestPruneRetiredXIDErrorsByAge covers the bound that keeps the retired-line
// set from growing for the life of the process: dmesg lines carry timestamps,
// so every line is distinct and nothing would ever be removed without it.
func TestPruneRetiredXIDErrorsByAge(t *testing.T) {
	e := newXIDTestExporter()
	now := time.Now()
	retention := 140 * time.Second

	e.retiredXIDErrors = map[string]time.Time{
		"fresh":      now.Add(-10 * time.Second),
		"borderline": now.Add(-retention + time.Second),
		"expired":    now.Add(-retention - time.Second),
		"long gone":  now.Add(-time.Hour),
		"just sent":  now,
	}

	e.pruneRetiredXIDErrors(now, retention)

	for _, keep := range []string{"fresh", "borderline", "just sent"} {
		if _, ok := e.retiredXIDErrors[keep]; !ok {
			t.Errorf("%q was pruned but is still inside the retention window", keep)
		}
	}
	for _, drop := range []string{"expired", "long gone"} {
		if _, ok := e.retiredXIDErrors[drop]; ok {
			t.Errorf("%q outlived the retention window but was not pruned", drop)
		}
	}
}

// TestPruneRetiredXIDErrorsSizeBackstop covers the cap that applies when a single
// dmesg window carries more distinct lines than age-based pruning retires. The
// newest records must survive, since those are the ones still able to reappear
// in the dmesg look-back window and be re-shipped as duplicates.
func TestPruneRetiredXIDErrorsSizeBackstop(t *testing.T) {
	e := newXIDTestExporter()
	now := time.Now()

	const excess = 10
	e.retiredXIDErrors = make(map[string]time.Time, maxRetiredXIDErrors+excess)
	for i := 0; i < maxRetiredXIDErrors+excess; i++ {
		// Higher i == more recent.
		e.retiredXIDErrors[fmt.Sprintf("line %d", i)] = now.Add(time.Duration(i) * time.Millisecond)
	}

	// Retention long enough that nothing is pruned by age; only the cap acts.
	e.pruneRetiredXIDErrors(now, time.Hour)

	if len(e.retiredXIDErrors) != maxRetiredXIDErrors {
		t.Fatalf("set size = %d, want %d", len(e.retiredXIDErrors), maxRetiredXIDErrors)
	}
	// The `excess` oldest entries are the ones that should be gone.
	for i := 0; i < excess; i++ {
		if _, ok := e.retiredXIDErrors[fmt.Sprintf("line %d", i)]; ok {
			t.Errorf("oldest record %q survived the size backstop", fmt.Sprintf("line %d", i))
		}
	}
	newest := fmt.Sprintf("line %d", maxRetiredXIDErrors+excess-1)
	if _, ok := e.retiredXIDErrors[newest]; !ok {
		t.Errorf("newest record %q was evicted", newest)
	}
}

// TestPruneRetiredXIDErrorsEmpty guards the nil-map path: pruning runs every tick,
// including before any XID has ever been delivered.
func TestPruneRetiredXIDErrorsEmpty(t *testing.T) {
	e := newXIDTestExporter()
	e.pruneRetiredXIDErrors(time.Now(), time.Minute)
	if len(e.retiredXIDErrors) != 0 {
		t.Fatalf("set size = %d, want 0", len(e.retiredXIDErrors))
	}
}

// TestTrimUnsentXIDErrorsRetiresDropped is the regression guard for the
// re-admission bug: evicted lines must be recorded as retired. A line that is
// neither pending nor retired looks fresh to the next getXIDErrors pass while
// it is still inside the overlapping dmesg window.
func TestTrimUnsentXIDErrorsRetiresDropped(t *testing.T) {
	e := newXIDTestExporter()

	const overflow = 3
	for i := 0; i < maxUnsentXIDErrors+overflow; i++ {
		e.unsentXIDErrors = append(e.unsentXIDErrors, fmt.Sprintf("Xid line %d", i))
	}

	e.trimUnsentXIDErrors()

	for i := 0; i < overflow; i++ {
		line := fmt.Sprintf("Xid line %d", i)
		if _, retired := e.retiredXIDErrors[line]; !retired {
			t.Errorf("dropped line %q was not retired; the next tick would re-admit it", line)
		}
	}
	// Surviving lines are still pending, so they must NOT be marked retired.
	if _, retired := e.retiredXIDErrors[fmt.Sprintf("Xid line %d", overflow)]; retired {
		t.Error("a line still in the pending buffer was marked retired")
	}
}

// TestXIDBufferNoReadmissionThrash reproduces the full multi-tick failure the
// retire-on-drop fix prevents. It models the deployed tickPeriod=60 case: an
// XID storm larger than the buffer, a host that stays unreachable, and a dmesg
// window that keeps re-reporting every line.
//
// Without retiring dropped lines: tick 1 keeps the newest 128 and drops the
// oldest, tick 2 re-admits those dropped lines at the tail and evicts the ones
// that are now at the head — which are newer — so DroppedCount climbs on every
// tick and the buffer ends up holding the OLDEST lines instead of the newest.
func TestXIDBufferNoReadmissionThrash(t *testing.T) {
	e := newXIDTestExporter()

	const storm = maxUnsentXIDErrors + 72
	candidates := make([]string, storm)
	for i := range candidates {
		candidates[i] = fmt.Sprintf("Xid line %03d", i)
	}

	// admit mirrors getXIDErrors' admission rules without shelling out to dmesg.
	admit := func() {
		for _, line := range candidates {
			if _, retired := e.retiredXIDErrors[line]; retired {
				continue
			}
			if slices.Contains(e.unsentXIDErrors, line) {
				continue
			}
			e.unsentXIDErrors = append(e.unsentXIDErrors, line)
		}
		e.trimUnsentXIDErrors()
	}

	admit()
	afterFirst := e.xidErrorsDropped
	if afterFirst != storm-maxUnsentXIDErrors {
		t.Fatalf("first tick dropped %d, want %d", afterFirst, storm-maxUnsentXIDErrors)
	}

	// Every subsequent tick sees the same dmesg window and must be a no-op:
	// nothing new to admit, so nothing further to drop.
	for tick := 2; tick <= 5; tick++ {
		admit()
		if e.xidErrorsDropped != afterFirst {
			t.Fatalf("tick %d: xidErrorsDropped grew to %d (want %d) — dropped lines are being re-admitted and re-counted",
				tick, e.xidErrorsDropped, afterFirst)
		}
	}

	// The buffer must still hold the NEWEST lines, not the re-admitted oldest.
	if len(e.unsentXIDErrors) != maxUnsentXIDErrors {
		t.Fatalf("buffer len = %d, want %d", len(e.unsentXIDErrors), maxUnsentXIDErrors)
	}
	wantHead := fmt.Sprintf("Xid line %03d", storm-maxUnsentXIDErrors)
	if e.unsentXIDErrors[0] != wantHead {
		t.Errorf("buffer head = %q, want %q — oldest-first eviction was inverted",
			e.unsentXIDErrors[0], wantHead)
	}
	wantTail := fmt.Sprintf("Xid line %03d", storm-1)
	if got := e.unsentXIDErrors[len(e.unsentXIDErrors)-1]; got != wantTail {
		t.Errorf("buffer tail = %q, want %q — the newest line must survive", got, wantTail)
	}
}
