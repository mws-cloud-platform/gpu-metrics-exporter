package gpumetricsexporter

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// XID/SXID lines come from the kernel log, read through /dev/kmsg rather than
// by running dmesg. Root in the guest can swap out a binary the exporter execs,
// but not the parsing compiled into the exporter, which pci-attest measures.
// The raw records also carry what dmesg's text output drops: the facility,
// which tells what the kernel logged from what userspace wrote into /dev/kmsg,
// and a sequence number, which replaces dmesg's per-tick time window.

const (
	kmsgPath = "/dev/kmsg"

	// /dev/kmsg is character device 1:11 on every Linux (drivers/char/mem.c).
	kmsgMajor = 1
	kmsgMinor = 11

	// kmsgReadBufSize holds any record /dev/kmsg hands out: the kernel caps a
	// record at 8 KiB before 6.1 (CONSOLE_EXT_LOG_MAX) and at 2 KiB since
	// (PRINTK_MESSAGE_MAX). A smaller buffer does not just fail the read: the
	// kernel has already moved past the record when it returns EINVAL.
	kmsgReadBufSize = 8192

	// kmsgMaxReadsPerDrain bounds one drain, so a tick finishes even if the
	// kernel were logging faster than the exporter reads; whatever is left
	// waits for the next tick. It is well above what a ring buffer holds.
	kmsgMaxReadsPerDrain = 1 << 16

	// Syslog facility and level numbers, as in <sys/syslog.h>.
	logFacilityKern = 0
	logLevelErr     = 3

	usecPerSec = 1_000_000
)

// kmsgRecord is one record as /dev/kmsg hands it out:
//
//	<pri>,<seq>,<usec>,<flags>[,<more>...];<text>\n[ <KEY>=<value>\n]...
//
// pri packs facility<<3 | level, seq numbers the records since boot, and usec
// is the printk timestamp dmesg prints as [sec.usec]. text is the message as
// the kernel escaped it — non-printable bytes and backslashes as \xNN — so a
// record is always one line.
type kmsgRecord struct {
	facility uint64
	level    uint64
	seq      uint64
	usec     uint64
	text     string
}

// parseKmsgRecord parses what one read() of /dev/kmsg returned. Header fields
// after the flags and the dictionary lines after the text are extensions the
// ABI says to ignore (Documentation/ABI/testing/dev-kmsg).
func parseKmsgRecord(b []byte) (kmsgRecord, error) {
	header, rest, ok := bytes.Cut(b, []byte{';'})
	if !ok {
		return kmsgRecord{}, fmt.Errorf("kernel log record without a header: %q", truncate(string(b)))
	}
	fields := strings.Split(string(header), ",")
	if len(fields) < 4 {
		return kmsgRecord{}, fmt.Errorf("kernel log record header %q: %d fields, want at least 4", truncate(string(header)), len(fields))
	}
	var nums [3]uint64
	for i := range nums {
		n, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return kmsgRecord{}, fmt.Errorf("kernel log record header %q: %w", truncate(string(header)), err)
		}
		nums[i] = n
	}
	text, _, _ := bytes.Cut(rest, []byte{'\n'})
	return kmsgRecord{
		facility: nums[0] >> 3,
		level:    nums[0] & 7,
		seq:      nums[1],
		usec:     nums[2],
		text:     string(text),
	}, nil
}

// dmesgLine renders the record as dmesg prints it by default, so the lines on
// the wire look as they did when the exporter ran dmesg.
func (r kmsgRecord) dmesgLine() string {
	return fmt.Sprintf("[%5d.%06d] %s", r.usec/usecPerSec, r.usec%usecPerSec, r.text)
}

// xidMessage matches the error lines the NVIDIA drivers log for a GPU (Xid) or
// an NVSwitch (SXid). It is anchored at the start of the message because the
// kernel puts process names into lines of its own — an OOM kill logs "Killed
// process N (<comm>)" at err level — and a process can name itself anything,
// so matching "xid" anywhere in a line lets any process forge one.
var xidMessage = regexp.MustCompile(`^(?:NVRM: Xid|nvidia-nvswitch\d*: SXid) \(`)

// isXIDRecord reports whether a record is an XID/SXID error: logged by the
// kernel, at err level or worse, in the shape of the NVIDIA drivers' error
// lines. The facility check is what makes writing to /dev/kmsg useless for
// faking one: the kernel files whatever userspace writes there under LOG_USER
// and never lets it claim LOG_KERN.
func isXIDRecord(r kmsgRecord) bool {
	return r.facility == logFacilityKern && r.level <= logLevelErr && xidMessage.MatchString(r.text)
}

// checkKmsgDevice verifies that what was opened is the kernel log device. Root
// can bind-mount a FIFO or a file over /dev/kmsg, but the device number of the
// opened file cannot be faked short of changing the kernel.
func checkKmsgDevice(mode, major, minor uint32) error {
	if mode&syscall.S_IFMT == syscall.S_IFCHR && major == kmsgMajor && minor == kmsgMinor {
		return nil
	}
	return fmt.Errorf("%s is not the kernel log: want character device %d:%d, got mode %#o device %d:%d",
		kmsgPath, kmsgMajor, kmsgMinor, mode, major, minor)
}

// kmsgDevice is an open /dev/kmsg. Read returns one record per call,
// syscall.EAGAIN once nothing new is left, and syscall.EPIPE when records were
// overwritten before they were read; the read after that starts at the oldest
// record still held.
type kmsgDevice interface {
	Read(p []byte) (int, error)
	Close() error
}

// kmsgReader gives each tick the kernel log records written since the last
// one. It keeps /dev/kmsg open, so the kernel keeps its read position and
// every record is read once. The sequence number of the last record read lets
// a reopened device skip what was already seen, and turns any gap into a count
// of records lost. No clock is involved after startup. The dmesg window this
// replaces compared printk timestamps, which NTP does not adjust, against
// CLOCK_BOOTTIME, which it does; once the printk clock had fallen further
// behind than the window's slack, a line logged just after a tick was out of
// the window by the next one.
//
// It belongs to the query goroutine.
type kmsgReader struct {
	log       *zap.Logger
	open      func() (kmsgDevice, error)
	sinceBoot func() (time.Duration, error)

	dev         kmsgDevice
	buf         []byte
	lastOpenErr string // logged when it changes, not on every tick

	haveSeq bool // a record has been read, so lastSeq is set
	lastSeq uint64
	lost    int64 // records overwritten before they could be read, cumulative

	// Until the reader has caught up with the log once, it is reading the
	// backlog that predates this run, and skips records stamped before
	// skipBefore (usec since boot); see drain.
	caughtUp   bool
	skipSet    bool
	skipBefore uint64
}

func newKmsgReader(log *zap.Logger) *kmsgReader {
	return &kmsgReader{log: log, open: openKmsg, sinceBoot: kmsgSinceBoot}
}

// drain calls visit for every record logged since the previous drain.
//
// The first drain has no previous one, and starting from the oldest record in
// the ring buffer would re-ship lines that an earlier run of the exporter
// already delivered. So until the reader first catches up with the log, it
// skips records stamped more than lookback ago — the look-back the exporter
// used to give dmesg. Everything logged after that is visited whatever its
// stamp: ending the backlog at a timestamp instead would, on a printk clock
// lagging the system clock by more than lookback, never end it.
//
// A failed read closes the device; the next drain reopens it and skips the
// records already read. A record that does not parse fails the drain and is
// left behind; the gap it leaves in the sequence numbers counts it as lost.
func (r *kmsgReader) drain(lookback time.Duration, visit func(kmsgRecord)) error {
	if err := r.ensureOpen(); err != nil {
		return err
	}
	if !r.caughtUp && !r.skipSet {
		now, err := r.sinceBoot()
		if err != nil {
			return fmt.Errorf("reading the clock to skip the kernel log backlog: %w", err)
		}
		r.skipSet, r.skipBefore = true, 0
		if now > lookback {
			r.skipBefore = uint64((now - lookback) / time.Microsecond)
		}
	}

	var lost uint64
	defer func() {
		if lost > 0 {
			r.log.Warn("kernel log records were overwritten before they could be read, XID errors among them are lost",
				zap.Uint64("lost", lost), zap.Int64("lostTotal", r.lost))
		}
	}()

	for range kmsgMaxReadsPerDrain {
		n, err := r.dev.Read(r.buf)
		if err != nil {
			switch {
			case errors.Is(err, syscall.EAGAIN):
				r.caughtUp = true
				return nil
			case errors.Is(err, syscall.EINTR), errors.Is(err, syscall.EPIPE):
				// On EPIPE the device has moved to the oldest record still
				// held; the gap in sequence numbers counts what was lost.
				continue
			default:
				r.closeDevice()
				return fmt.Errorf("reading %s: %w", kmsgPath, err)
			}
		}
		rec, err := parseKmsgRecord(r.buf[:n])
		if err != nil {
			return err
		}
		if r.haveSeq {
			if rec.seq <= r.lastSeq {
				continue // read before the device was reopened
			}
			if gap := rec.seq - r.lastSeq - 1; gap > 0 {
				lost += gap
				r.lost += int64(gap)
			}
		}
		r.haveSeq, r.lastSeq = true, rec.seq
		if !r.caughtUp && rec.usec < r.skipBefore {
			continue
		}
		visit(rec)
	}
	return nil
}

func (r *kmsgReader) ensureOpen() error {
	if r.dev != nil {
		return nil
	}
	dev, err := r.open()
	if err != nil {
		// Every payload carries the error; the log gets it once.
		if msg := err.Error(); msg != r.lastOpenErr {
			r.lastOpenErr = msg
			r.log.Error("cannot open the kernel log, XID errors are not collected", zap.Error(err))
		}
		return err
	}
	r.lastOpenErr = ""
	r.log.Info("reading XID errors from the kernel log", zap.String("path", kmsgPath))
	r.dev = dev
	if r.buf == nil {
		r.buf = make([]byte, kmsgReadBufSize)
	}
	return nil
}

// closeDevice releases /dev/kmsg; a later drain would reopen it. Run calls it
// once the query loop has exited.
func (r *kmsgReader) closeDevice() {
	if r == nil || r.dev == nil {
		return
	}
	_ = r.dev.Close()
	r.dev = nil
}

// lostRecords is the cumulative number of records overwritten in the ring
// buffer before the reader got to them.
func (r *kmsgReader) lostRecords() int64 {
	return r.lost
}
