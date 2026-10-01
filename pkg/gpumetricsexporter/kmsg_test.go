package gpumetricsexporter

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Records as a live kernel handed them out of /dev/kmsg: a line root wrote into
// /dev/kmsg, and the OOM kill of a process named "oxide". The exporter's old
// filter — any dmesg -l err line containing "xid" — took both for XID errors.
const (
	injectedXidRecord = "11,22625,652513086584,-;NVRM: Xid (PCI:0000:00:05): 79, kmsg-injection-test\n"
	oomOxideRecord    = "3,22726,652513349062,-;Memory cgroup out of memory: Killed process 51807 (oxide) total-vm:412192kB, anon-rss:62936kB, file-rss:1824kB, shmem-rss:0kB, UID:0 pgtables:344kB oom_score_adj:0\n"
)

// Lines in the format the NVIDIA drivers log them.
const (
	xid79Text  = "NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus."
	sxidText   = "nvidia-nvswitch2: SXid (PCI:0000:8a:00.0): 12028, Non-fatal, Link 61 egress non-posted PRIV error (First)"
	oomPrefix  = "Memory cgroup out of memory: Killed process 51807 (oxide)"
	kmsgErrPri = 3 // kern.err
)

func mustParseKmsgRecord(t *testing.T, s string) kmsgRecord {
	t.Helper()
	r, err := parseKmsgRecord([]byte(s))
	if err != nil {
		t.Fatalf("parseKmsgRecord(%q): %v", s, err)
	}
	return r
}

func TestParseKmsgRecord(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want kmsgRecord
	}{
		{
			name: "written by userspace",
			in:   injectedXidRecord,
			want: kmsgRecord{facility: 1, level: 3, seq: 22625, usec: 652513086584, text: "NVRM: Xid (PCI:0000:00:05): 79, kmsg-injection-test"},
		},
		{
			name: "logged by the kernel",
			in:   oomOxideRecord,
			want: kmsgRecord{facility: 0, level: 3, seq: 22726, usec: 652513349062, text: strings.TrimSuffix(oomOxideRecord[strings.IndexByte(oomOxideRecord, ';')+1:], "\n")},
		},
		{
			// Fields after the flags and the dictionary lines are extensions
			// a reader must ignore.
			name: "caller field and dictionary",
			in:   "6,1093,4096000,-,caller=T1;nvidia 0000:3b:00.0: enabling device (0000 -> 0003)\n SUBSYSTEM=pci\n DEVICE=+pci:0000:3b:00.0\n",
			want: kmsgRecord{facility: 0, level: 6, seq: 1093, usec: 4096000, text: "nvidia 0000:3b:00.0: enabling device (0000 -> 0003)"},
		},
		{
			name: "semicolon in the text",
			in:   "6,1,2,-;a;b\n",
			want: kmsgRecord{facility: 0, level: 6, seq: 1, usec: 2, text: "a;b"},
		},
		{
			// The kernel escapes a newline inside a message, so a record stays
			// one line, and so does what the exporter ships.
			name: "escaped newline",
			in:   `3,5,6,c;NVRM: Xid (PCI:0000:3b:00): 13, name=a\x0ab` + "\n",
			want: kmsgRecord{facility: 0, level: 3, seq: 5, usec: 6, text: `NVRM: Xid (PCI:0000:3b:00): 13, name=a\x0ab`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustParseKmsgRecord(t, tc.in); got != tc.want {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}

	for _, bad := range []string{"", "no header\n", "6,1,2;three fields\n", "6,x,2,-;bad seq\n", "-1,1,2,-;negative\n"} {
		if r, err := parseKmsgRecord([]byte(bad)); err == nil {
			t.Errorf("parseKmsgRecord(%q) = %+v, want an error", bad, r)
		}
	}
}

// TestKmsgRecordDmesgLine pins the wire format of an XID line to what dmesg
// printed for the same records, so consumers see no change.
func TestKmsgRecordDmesgLine(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Both as `dmesg -l err` printed them on the kernel they came from.
		{injectedXidRecord, "[652513.086584] NVRM: Xid (PCI:0000:00:05): 79, kmsg-injection-test"},
		{oomOxideRecord, "[652513.349062] " + oomPrefix + " total-vm:412192kB, anon-rss:62936kB, file-rss:1824kB, shmem-rss:0kB, UID:0 pgtables:344kB oom_score_adj:0"},
		{"3,1,5000042,-;early\n", "[    5.000042] early"},
		{"3,0,0,-;first\n", "[    0.000000] first"},
	}
	for _, tc := range cases {
		if got := mustParseKmsgRecord(t, tc.in).dmesgLine(); got != tc.want {
			t.Errorf("dmesgLine() = %q, want %q", got, tc.want)
		}
	}
}

func TestIsXIDRecord(t *testing.T) {
	cases := []struct {
		name   string
		record string
		want   bool
	}{
		{"Xid", "3,1,1,-;" + xid79Text, true},
		{"SXid", "3,1,1,-;" + sxidText, true},
		{"SXid without an instance number", "3,1,1,-;nvidia-nvswitch: SXid (PCI:0000:8a:00.0): 12028, Non-fatal", true},
		{"Xid at crit", "2,1,1,-;" + xid79Text, true},
		{"Xid at warning", "4,1,1,-;" + xid79Text, false},
		{"written into /dev/kmsg by userspace", injectedXidRecord, false},
		{"OOM kill of a process named oxide", oomOxideRecord, false},
		// A process name is at most 15 bytes, exactly enough for this.
		{"OOM kill of a process named like an Xid line", "3,1,1,-;Out of memory: Killed process 4242 (NVRM: Xid (PCI:) total-vm:1024kB\n", false},
		{"NVRM line that is not an Xid", "3,1,1,-;NVRM: GPU 0000:3b:00.0: RmInitAdapter failed! (0x26:0x56:1474)\n", false},
		{"Xid mentioned mid-line", "3,1,1,-;nvidia-uvm: reported NVRM: Xid (PCI:0000:3b:00): 31\n", false},
		{"wrong case", "3,1,1,-;nvrm: xid (PCI:0000:3b:00): 79\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isXIDRecord(mustParseKmsgRecord(t, tc.record)); got != tc.want {
				t.Errorf("isXIDRecord(%q) = %v, want %v", tc.record, got, tc.want)
			}
		})
	}
}

func TestCheckKmsgDevice(t *testing.T) {
	cases := []struct {
		name         string
		mode         uint32
		major, minor uint32
		ok           bool
	}{
		{"kmsg", syscall.S_IFCHR | 0o644, 1, 11, true},
		{"/dev/null mounted over it", syscall.S_IFCHR | 0o666, 1, 3, false},
		{"FIFO", syscall.S_IFIFO | 0o600, 0, 0, false},
		{"regular file", syscall.S_IFREG | 0o644, 0, 0, false},
		{"block device 1:11", syscall.S_IFBLK | 0o600, 1, 11, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKmsgDevice(tc.mode, tc.major, tc.minor)
			if (err == nil) != tc.ok {
				t.Errorf("checkKmsgDevice(%#o, %d, %d) = %v, want ok=%v", tc.mode, tc.major, tc.minor, err, tc.ok)
			}
		})
	}
}

// fakeKernelLog models the ring buffer behind /dev/kmsg: records keep their
// sequence numbers, the oldest are overwritten once it is full, and each open
// starts at the oldest record still held, as the kernel's does.
type fakeKernelLog struct {
	held     []kmsgRecord
	nextSeq  uint64
	capacity int               // 0: never overwrites
	raw      map[uint64]string // records handed out as given instead of rendered
	openErr  error
	files    []*fakeKmsgFile
}

func (l *fakeKernelLog) add(pri uint64, stamp time.Duration, text string) {
	l.held = append(l.held, kmsgRecord{facility: pri >> 3, level: pri & 7, seq: l.nextSeq, usec: uint64(stamp / time.Microsecond), text: text})
	l.nextSeq++
	if l.capacity > 0 && len(l.held) > l.capacity {
		l.held = l.held[len(l.held)-l.capacity:]
	}
}

func (l *fakeKernelLog) open() (kmsgDevice, error) {
	if l.openErr != nil {
		return nil, l.openErr
	}
	f := &fakeKmsgFile{log: l, next: l.nextSeq}
	if len(l.held) > 0 {
		f.next = l.held[0].seq
	}
	l.files = append(l.files, f)
	return f, nil
}

type fakeKmsgFile struct {
	log     *fakeKernelLog
	next    uint64 // the kernel's per-open read position
	readErr error  // returned once, by the next Read
	closed  bool
}

func (f *fakeKmsgFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, syscall.EBADF
	}
	if err := f.readErr; err != nil {
		f.readErr = nil
		return 0, err
	}
	held := f.log.held
	if f.next >= f.log.nextSeq {
		return 0, syscall.EAGAIN
	}
	if f.next < held[0].seq {
		f.next = held[0].seq
		return 0, syscall.EPIPE
	}
	r := held[f.next-held[0].seq]
	f.next++
	if raw, ok := f.log.raw[r.seq]; ok {
		return copy(p, raw), nil
	}
	return copy(p, fmt.Sprintf("%d,%d,%d,-;%s\n", r.facility<<3|r.level, r.seq, r.usec, r.text)), nil
}

func (f *fakeKmsgFile) Close() error {
	f.closed = true
	return nil
}

// newTestKmsgReader reads l with the clock stopped at *now since boot.
func newTestKmsgReader(l *fakeKernelLog, log *zap.Logger, now *time.Duration) *kmsgReader {
	return &kmsgReader{
		log:       log,
		open:      l.open,
		sinceBoot: func() (time.Duration, error) { return *now, nil },
	}
}

// drainTexts runs one drain and returns the text of every record visited.
func drainTexts(t *testing.T, r *kmsgReader, lookback time.Duration) []string {
	t.Helper()
	var got []string
	if err := r.drain(lookback, func(rec kmsgRecord) { got = append(got, rec.text) }); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return got
}

func noVisit(t *testing.T) func(kmsgRecord) {
	return func(rec kmsgRecord) { t.Errorf("visited %+v", rec) }
}

// TestKmsgReaderStartupBacklog covers the first drain: the ring buffer holds
// lines from long before this run, which an earlier run already shipped, so
// only the look-back window of them is visited; after that, every new record
// and nothing else.
func TestKmsgReaderStartupBacklog(t *testing.T) {
	l := &fakeKernelLog{}
	l.add(kmsgErrPri, 5*time.Second, "boot")
	l.add(kmsgErrPri, 900*time.Second, "before the look-back")
	l.add(kmsgErrPri, 940*time.Second, "inside the look-back")
	l.add(kmsgErrPri, 990*time.Second, "just before start")
	now := 1000 * time.Second
	r := newTestKmsgReader(l, zap.NewNop(), &now)

	if got, want := drainTexts(t, r, 70*time.Second), []string{"inside the look-back", "just before start"}; !slices.Equal(got, want) {
		t.Fatalf("first drain visited %q, want %q", got, want)
	}
	if got := drainTexts(t, r, 70*time.Second); len(got) != 0 {
		t.Fatalf("drain with nothing new visited %q", got)
	}
	l.add(kmsgErrPri, 1010*time.Second, "new")
	now = 1060 * time.Second
	if got, want := drainTexts(t, r, 70*time.Second), []string{"new"}; !slices.Equal(got, want) {
		t.Fatalf("second drain visited %q, want %q", got, want)
	}
}

// TestKmsgReaderNoClockAfterStartup is the failure the dmesg window had: printk
// stamps records on a clock NTP does not adjust, so on a long-running guest a
// record logged just now can carry a stamp older than any window measured on
// the system clock. Once past the startup backlog, the reader must not look at
// stamps at all.
func TestKmsgReaderNoClockAfterStartup(t *testing.T) {
	l := &fakeKernelLog{}
	l.add(kmsgErrPri, 500*time.Second, "backlog")
	now := 1000 * time.Second
	r := newTestKmsgReader(l, zap.NewNop(), &now)
	if got := drainTexts(t, r, 70*time.Second); len(got) != 0 {
		t.Fatalf("first drain visited %q, want nothing from the backlog", got)
	}

	// Logged after startup, but stamped as if 400 s ago: the printk clock lags.
	l.add(kmsgErrPri, 600*time.Second, "logged now, stamped late")
	now = 1060 * time.Second
	if got, want := drainTexts(t, r, 70*time.Second), []string{"logged now, stamped late"}; !slices.Equal(got, want) {
		t.Fatalf("drain visited %q, want %q", got, want)
	}
}

// TestKmsgReaderReopenReadsEachRecordOnce covers a failed read: the device is
// closed, and the reopened one starts from the oldest record again, which the
// reader must skip up to where it was.
func TestKmsgReaderReopenReadsEachRecordOnce(t *testing.T) {
	l := &fakeKernelLog{}
	l.add(kmsgErrPri, 10*time.Second, "one")
	l.add(kmsgErrPri, 11*time.Second, "two")
	now := 20 * time.Second
	r := newTestKmsgReader(l, zap.NewNop(), &now)

	if got, want := drainTexts(t, r, time.Minute), []string{"one", "two"}; !slices.Equal(got, want) {
		t.Fatalf("first drain visited %q, want %q", got, want)
	}

	l.add(kmsgErrPri, 21*time.Second, "three")
	l.files[0].readErr = syscall.EIO
	if err := r.drain(time.Minute, noVisit(t)); !errors.Is(err, syscall.EIO) {
		t.Fatalf("drain error = %v, want EIO", err)
	}
	if !l.files[0].closed {
		t.Fatal("device left open after a failed read")
	}

	l.add(kmsgErrPri, 22*time.Second, "four")
	if got, want := drainTexts(t, r, time.Minute), []string{"three", "four"}; !slices.Equal(got, want) {
		t.Fatalf("drain after reopening visited %q, want %q", got, want)
	}
	if len(l.files) != 2 {
		t.Fatalf("device opened %d times, want 2", len(l.files))
	}
	if r.lostRecords() != 0 {
		t.Fatalf("lostRecords = %d, want 0: nothing was overwritten", r.lostRecords())
	}
}

// TestKmsgReaderCountsOverwrittenRecords covers a log storm that wraps the
// ring buffer between two ticks: the read reports EPIPE and resumes at the
// oldest record held, and the records in between are counted lost, with one
// warning per drain.
func TestKmsgReaderCountsOverwrittenRecords(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	l := &fakeKernelLog{capacity: 4}
	l.add(kmsgErrPri, time.Second, "before the storm")
	now := 2 * time.Second
	r := newTestKmsgReader(l, zap.New(core), &now)
	drainTexts(t, r, time.Minute)

	for i := range 10 {
		l.add(kmsgErrPri, 3*time.Second, fmt.Sprintf("storm %d", i))
	}
	want := []string{"storm 6", "storm 7", "storm 8", "storm 9"}
	if got := drainTexts(t, r, time.Minute); !slices.Equal(got, want) {
		t.Fatalf("drain visited %q, want %q", got, want)
	}
	if r.lostRecords() != 6 {
		t.Fatalf("lostRecords = %d, want 6", r.lostRecords())
	}

	l.add(kmsgErrPri, 4*time.Second, "after")
	drainTexts(t, r, time.Minute)
	if r.lostRecords() != 6 {
		t.Fatalf("lostRecords = %d after a drain that lost nothing, want it to stay 6", r.lostRecords())
	}
	if n := logs.FilterMessageSnippet("overwritten").Len(); n != 1 {
		t.Fatalf("logged the loss %d times, want once", n)
	}
}

// TestKmsgReaderOpenFailure covers a kernel log that cannot be opened (no
// device, or no CAP_SYSLOG): every drain reports it, the log says it once, and
// once the device appears the backlog still gets its look-back.
func TestKmsgReaderOpenFailure(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	l := &fakeKernelLog{openErr: &os.PathError{Op: "open", Path: kmsgPath, Err: syscall.EACCES}}
	l.add(kmsgErrPri, 10*time.Second, "long before")
	l.add(kmsgErrPri, 95*time.Second, "logged while unreadable")
	now := 100 * time.Second
	r := newTestKmsgReader(l, zap.New(core), &now)

	for range 3 {
		if err := r.drain(time.Minute, noVisit(t)); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("drain error = %v, want EACCES", err)
		}
	}
	if n := logs.FilterMessageSnippet("cannot open the kernel log").Len(); n != 1 {
		t.Fatalf("open failure logged %d times over 3 drains, want once", n)
	}

	l.openErr = nil
	if got, want := drainTexts(t, r, time.Minute), []string{"logged while unreadable"}; !slices.Equal(got, want) {
		t.Fatalf("drain visited %q, want %q", got, want)
	}
}

// TestKmsgReaderUnparsableRecord covers a record that does not parse: the
// drain fails, the reader moves on, and the record counts as lost.
func TestKmsgReaderUnparsableRecord(t *testing.T) {
	l := &fakeKernelLog{raw: map[uint64]string{1: "garbage\n"}}
	l.add(kmsgErrPri, time.Second, "before")
	l.add(kmsgErrPri, time.Second, "unparsable")
	l.add(kmsgErrPri, time.Second, "after")
	now := 2 * time.Second
	r := newTestKmsgReader(l, zap.NewNop(), &now)

	var got []string
	if err := r.drain(time.Minute, func(rec kmsgRecord) { got = append(got, rec.text) }); err == nil {
		t.Fatal("drain over an unparsable record returned no error")
	}
	got = append(got, drainTexts(t, r, time.Minute)...)
	if want := []string{"before", "after"}; !slices.Equal(got, want) {
		t.Fatalf("visited %q, want %q", got, want)
	}
	if r.lostRecords() != 1 {
		t.Fatalf("lostRecords = %d, want 1", r.lostRecords())
	}
}

// TestKmsgReaderReadBound covers a log that keeps the reader busy past one
// drain's bound: the drain ends, still inside the startup backlog, and the
// next one carries on from there.
func TestKmsgReaderReadBound(t *testing.T) {
	l := &fakeKernelLog{}
	for range kmsgMaxReadsPerDrain + 2 {
		l.add(kmsgErrPri, time.Second, "old")
	}
	l.add(kmsgErrPri, 100*time.Second, "recent")
	now := 101 * time.Second
	r := newTestKmsgReader(l, zap.NewNop(), &now)

	if got := drainTexts(t, r, 10*time.Second); len(got) != 0 {
		t.Fatalf("first drain visited %d records, want none", len(got))
	}
	if r.caughtUp {
		t.Fatal("caught up after a drain that stopped at its bound")
	}
	if got, want := drainTexts(t, r, 10*time.Second), []string{"recent"}; !slices.Equal(got, want) {
		t.Fatalf("second drain visited %q, want %q", got, want)
	}
}
