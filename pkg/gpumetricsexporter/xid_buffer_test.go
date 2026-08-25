package gpumetricsexporter

import (
	"fmt"
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

// TestPruneSentXIDErrorsByAge covers the bound that keeps the delivered-line
// set from growing for the life of the process: dmesg lines carry timestamps,
// so every line is distinct and nothing would ever be removed without it.
func TestPruneSentXIDErrorsByAge(t *testing.T) {
	e := newXIDTestExporter()
	now := time.Now()
	retention := 140 * time.Second

	e.sentXIDErrors = map[string]time.Time{
		"fresh":      now.Add(-10 * time.Second),
		"borderline": now.Add(-retention + time.Second),
		"expired":    now.Add(-retention - time.Second),
		"long gone":  now.Add(-time.Hour),
		"just sent":  now,
	}

	e.pruneSentXIDErrors(now, retention)

	for _, keep := range []string{"fresh", "borderline", "just sent"} {
		if _, ok := e.sentXIDErrors[keep]; !ok {
			t.Errorf("%q was pruned but is still inside the retention window", keep)
		}
	}
	for _, drop := range []string{"expired", "long gone"} {
		if _, ok := e.sentXIDErrors[drop]; ok {
			t.Errorf("%q outlived the retention window but was not pruned", drop)
		}
	}
}

// TestPruneSentXIDErrorsSizeBackstop covers the cap that applies when a single
// dmesg window carries more distinct lines than age-based pruning retires. The
// newest records must survive, since those are the ones still able to reappear
// in the dmesg look-back window and be re-shipped as duplicates.
func TestPruneSentXIDErrorsSizeBackstop(t *testing.T) {
	e := newXIDTestExporter()
	now := time.Now()

	const excess = 10
	e.sentXIDErrors = make(map[string]time.Time, maxSentXIDErrors+excess)
	for i := 0; i < maxSentXIDErrors+excess; i++ {
		// Higher i == more recent.
		e.sentXIDErrors[fmt.Sprintf("line %d", i)] = now.Add(time.Duration(i) * time.Millisecond)
	}

	// Retention long enough that nothing is pruned by age; only the cap acts.
	e.pruneSentXIDErrors(now, time.Hour)

	if len(e.sentXIDErrors) != maxSentXIDErrors {
		t.Fatalf("set size = %d, want %d", len(e.sentXIDErrors), maxSentXIDErrors)
	}
	// The `excess` oldest entries are the ones that should be gone.
	for i := 0; i < excess; i++ {
		if _, ok := e.sentXIDErrors[fmt.Sprintf("line %d", i)]; ok {
			t.Errorf("oldest record %q survived the size backstop", fmt.Sprintf("line %d", i))
		}
	}
	newest := fmt.Sprintf("line %d", maxSentXIDErrors+excess-1)
	if _, ok := e.sentXIDErrors[newest]; !ok {
		t.Errorf("newest record %q was evicted", newest)
	}
}

// TestPruneSentXIDErrorsEmpty guards the nil-map path: pruning runs every tick,
// including before any XID has ever been delivered.
func TestPruneSentXIDErrorsEmpty(t *testing.T) {
	e := newXIDTestExporter()
	e.pruneSentXIDErrors(time.Now(), time.Minute)
	if len(e.sentXIDErrors) != 0 {
		t.Fatalf("set size = %d, want 0", len(e.sentXIDErrors))
	}
}
