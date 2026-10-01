package gpumetricsexporter

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
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

// newKmsgTestExporter builds an exporter whose kernel log is l, read with the
// clock stopped at *now since boot.
func newKmsgTestExporter(l *fakeKernelLog, now *time.Duration) *GpuMetricsExporter {
	e := newXIDTestExporter()
	e.config.TickPeriod = time.Minute
	e.kmsg = newTestKmsgReader(l, e.log, now)
	return e
}

// TestGetXIDErrorsAdmitsOnlyGenuineLines runs the lines the dmesg-based filter
// took for XIDs through the whole collection path, next to genuine ones.
func TestGetXIDErrorsAdmitsOnlyGenuineLines(t *testing.T) {
	l := &fakeKernelLog{}
	l.add(kmsgErrPri, 941*time.Second, xid79Text)
	l.add(1<<3|3, 942*time.Second, "NVRM: Xid (PCI:0000:00:05): 79, kmsg-injection-test") // user.err: written into /dev/kmsg
	l.add(kmsgErrPri, 943*time.Second, oomPrefix+" total-vm:412192kB")
	l.add(6, 944*time.Second, "NVRM: loading NVIDIA UNIX x86_64 Kernel Module  550.54.15")
	l.add(kmsgErrPri, 945*time.Second, sxidText)
	now := 1000 * time.Second
	e := newKmsgTestExporter(l, &now)

	got, err := e.getXIDErrors()
	if err != nil {
		t.Fatalf("getXIDErrors: %v", err)
	}
	if want := []string{"[  941.000000] " + xid79Text, "[  945.000000] " + sxidText}; !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// TestGetXIDErrorsReshipsUntilDelivered covers the delivery contract on top of
// the reader: a line rides on every payload until one carrying it is
// delivered, and is read from the kernel log only once.
func TestGetXIDErrorsReshipsUntilDelivered(t *testing.T) {
	l := &fakeKernelLog{}
	l.add(kmsgErrPri, 990*time.Second, xid79Text)
	now := 1000 * time.Second
	e := newKmsgTestExporter(l, &now)
	tick := func() []string {
		t.Helper()
		got, err := e.getXIDErrors()
		if err != nil {
			t.Fatalf("getXIDErrors: %v", err)
		}
		return got
	}

	first := "[  990.000000] " + xid79Text
	for range 2 { // the host is unreachable: the line rides again, once
		if got := tick(); !slices.Equal(got, []string{first}) {
			t.Fatalf("got %q, want %q", got, []string{first})
		}
	}

	l.add(kmsgErrPri, 1050*time.Second, sxidText)
	payload := tick()
	if want := []string{first, "[ 1050.000000] " + sxidText}; !slices.Equal(payload, want) {
		t.Fatalf("got %q, want %q", payload, want)
	}

	// What sendMetrics does once that payload is delivered.
	e.unsentXIDErrors = removeStrings(e.unsentXIDErrors, payload)
	if got := tick(); got != nil {
		t.Fatalf("after delivery got %q, want nothing", got)
	}
}

// TestGetXIDErrorsShipsPendingWhenReadFails covers a tick whose kernel log read
// fails: the lines already pending still ship, next to the error.
func TestGetXIDErrorsShipsPendingWhenReadFails(t *testing.T) {
	l := &fakeKernelLog{}
	l.add(kmsgErrPri, 990*time.Second, xid79Text)
	now := 1000 * time.Second
	e := newKmsgTestExporter(l, &now)
	if _, err := e.getXIDErrors(); err != nil {
		t.Fatalf("getXIDErrors: %v", err)
	}

	l.files[0].readErr = syscall.EIO
	got, err := e.getXIDErrors()
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("getXIDErrors error = %v, want EIO", err)
	}
	if want := []string{"[  990.000000] " + xid79Text}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestXIDStormOverflowCountedOnce covers an XID storm larger than the pending
// buffer while the host is unreachable: the overflow is dropped oldest-first
// and counted once, and later ticks, with nothing new in the kernel log, drop
// and count nothing more. With the dmesg window every tick re-read the same
// lines, so a dropped line had to be remembered to keep it from coming back.
func TestXIDStormOverflowCountedOnce(t *testing.T) {
	const storm = maxUnsentXIDErrors + 72
	line := func(i int) string { return fmt.Sprintf("NVRM: Xid (PCI:0000:3b:00): 74, line %03d", i) }
	l := &fakeKernelLog{}
	for i := range storm {
		l.add(kmsgErrPri, 990*time.Second, line(i))
	}
	now := 1000 * time.Second
	e := newKmsgTestExporter(l, &now)

	for tick := 1; tick <= 5; tick++ {
		if _, err := e.getXIDErrors(); err != nil {
			t.Fatalf("getXIDErrors: %v", err)
		}
		if e.xidErrorsDropped != storm-maxUnsentXIDErrors {
			t.Fatalf("tick %d: xidErrorsDropped = %d, want %d", tick, e.xidErrorsDropped, storm-maxUnsentXIDErrors)
		}
	}
	if len(e.unsentXIDErrors) != maxUnsentXIDErrors {
		t.Fatalf("buffer len = %d, want %d", len(e.unsentXIDErrors), maxUnsentXIDErrors)
	}
	if want := "[  990.000000] " + line(storm-maxUnsentXIDErrors); e.unsentXIDErrors[0] != want {
		t.Errorf("buffer head = %q, want %q: the oldest lines go first", e.unsentXIDErrors[0], want)
	}
	if want := "[  990.000000] " + line(storm-1); e.unsentXIDErrors[maxUnsentXIDErrors-1] != want {
		t.Errorf("buffer tail = %q, want %q: the newest line must survive", e.unsentXIDErrors[maxUnsentXIDErrors-1], want)
	}
}

// TestQueryMetricsReportsXIDErrors pins the wiring into the payload: the
// pending lines next to a read error, and the count of records lost.
func TestQueryMetricsReportsXIDErrors(t *testing.T) {
	p := newFakeProcess(t)
	e := NewGpuMetricsExporter(GpuMetricsExporterConfig{
		Log:            zap.NewNop(),
		TickPeriod:     time.Minute,
		InstanceIDPath: filepath.Join(p.root, "instance-id"),
	})
	e.initNVMLFn = func() error { return errors.New("no NVML here") }
	e.nvmlLib = p.inspector()
	l := &fakeKernelLog{capacity: 2}
	l.add(kmsgErrPri, 990*time.Second, xid79Text)
	now := 1000 * time.Second
	e.kmsg = newTestKmsgReader(l, e.log, &now)
	want := []string{"[  990.000000] " + xid79Text}

	tick := func() *gpumetrics.XIDErrors {
		t.Helper()
		m, err := e.queryMetrics()
		if err != nil {
			t.Fatalf("queryMetrics: %v", err)
		}
		return &m.XIDErrors
	}

	if x := tick(); !slices.Equal(x.XIDErrors, want) || x.Error != "" || x.KernelLogLostCount != 0 {
		t.Fatalf("first tick: %+v", x)
	}

	// Three records arrive and the ring buffer keeps two: one is lost.
	for range 3 {
		l.add(6, 995*time.Second, "noise")
	}
	if x := tick(); x.KernelLogLostCount != 1 {
		t.Fatalf("after the ring buffer wrapped: %+v, want kernel_log_lost_count 1", x)
	}

	l.files[0].readErr = syscall.EIO
	x := tick()
	if x.Error == "" || !slices.Equal(x.XIDErrors, want) || x.KernelLogLostCount != 1 {
		t.Fatalf("tick with a failed read: %+v, want the error, the pending line and the loss count", x)
	}
}
