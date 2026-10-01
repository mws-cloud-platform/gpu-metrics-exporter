package attest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	pciVendor = 0x1234
	pciDevice = 0x11e9

	barSize = 0x1000

	// A bare doorbell: the only readable register is STATUS, which carries the
	// protocol version in its high half and the busy bit.  Found by PCI id.
	regStatus    = 0x0c
	regCmd       = 0x10
	regLength    = 0x14
	regDataPtrLo = 0x18
	regDataPtrHi = 0x1c

	stBusy         = 1 << 1
	stVersionShift = 16

	cmdSend = 2

	// Protocol version this agent speaks; refuse a device that reports another.
	protoVersion = 1

	// The device clamps a longer send; keep our buffer within that ceiling.
	maxPayload = 64 * 1024

	// busyPollInterval paces the wait for busy to clear.  Under KVM busy is
	// normally clear by the first read; the sleep only matters for a slow
	// (TCG) or stuck device, which a tight loop would hammer with MMIO exits
	// for the whole timeout.
	busyPollInterval = time.Millisecond
)

// ErrNoDevice reports that the guest has no pci-attest device -- the normal
// case under a QEMU without one, unlike a device that is there but could not
// be opened.
var ErrNoDevice = errors.New("no pci-attest device")

// Info describes an open device.
type Info struct {
	Slot string // PCI slot, e.g. "0000:00:04.0"
}

// Device is an open pci-attest device.  Send is safe for concurrent use.
type Device struct {
	info Info
	bar  []byte // mmap of BAR 0 (registers)
	buf  []byte // pinned, off-heap payload staging buffer
	mu   sync.Mutex
}

// Open finds the pci-attest device, maps its registers, pins a payload
// buffer, and pins this binary's own code and note pages resident so the
// device's per-send measurement finds every page.  Needs root: mmap()ing a
// BAR through sysfs requires CAP_SYS_RAWIO, and mlock needs RLIMIT_MEMLOCK.
func Open() (*Device, error) {
	slot, err := findDevice()
	if err != nil {
		return nil, err
	}
	enableMemoryDecode(slot)

	path := filepath.Join("/sys/bus/pci/devices", slot, "resource0")
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_SYNC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	bar, err := syscall.Mmap(int(f.Fd()), 0, barSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap BAR: %w", err)
	}

	// The PCI id already identified the device; the only thing to check is
	// that it speaks a protocol ABI we understand.
	d := &Device{bar: bar, info: Info{Slot: slot}}
	if err := d.checkProtocol(); err != nil {
		_ = syscall.Munmap(bar)
		return nil, fmt.Errorf("%s: %w", slot, err)
	}

	// The payload lives in its own anonymous mapping, outside the Go heap so
	// its address never moves, sized to the device ceiling and mlock'd so the
	// device -- reading it through our page tables while the guest is stopped
	// -- finds every page.
	buf, err := syscall.Mmap(-1, 0, maxPayload,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		_ = syscall.Munmap(bar)
		return nil, fmt.Errorf("mmap payload buffer: %w", err)
	}
	if err := syscall.Mlock(buf); err != nil {
		_ = syscall.Munmap(buf)
		_ = syscall.Munmap(bar)
		return nil, fmt.Errorf("mlock payload buffer: %w", err)
	}
	d.buf = buf

	// Pin our own code and note pages: a swapped-out page reads back to the
	// device as absent and the send is silently dropped.
	if err := PinImage(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("pin image: %w", err)
	}
	return d, nil
}

// Close unmaps everything.  It does not unpin the image (other Devices, and
// the running code itself, may still rely on it).  It takes the same lock as
// Send, so a concurrent Send finishes before the mappings go away rather than
// writing into freed memory, and Close is safe to call more than once.
func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.buf != nil {
		_ = syscall.Munlock(d.buf)
		_ = syscall.Munmap(d.buf)
		d.buf = nil
	}
	if d.bar != nil {
		_ = syscall.Munmap(d.bar)
		d.bar = nil
	}
	return nil
}

// checkProtocol refuses a device whose STATUS[31:16] reports a protocol
// version other than the one this package speaks, rather than drive a
// register layout that may have changed under it.  A device from before the
// version existed reads 0 there.  busy, in the low half, does not matter.
func (d *Device) checkProtocol() error {
	if ver := d.rd(regStatus) >> stVersionShift; ver != protoVersion {
		return fmt.Errorf("device protocol v%d, this agent speaks v%d", ver, protoVersion)
	}
	return nil
}

// Info returns the device's static description.
func (d *Device) Info() Info { return d.info }

// MaxPayload is the largest payload Send will accept.
func (d *Device) MaxPayload() int { return maxPayload }

// Send hands one payload to the device, which attests the calling process and,
// on a match, keeps the bytes for the host to read over QMP.  The guest is
// write-only: the device does not report whether it kept or dropped the
// payload, so Send returns an error only for a local problem (an empty or
// oversize payload, a device that has been Closed, or one that stayed busy
// past the timeout).  Whether a payload arrives is the host's to observe --
// the signed binary's sends advance the device's last-seq, everything else is
// dropped in silence.
//
// timeout bounds the whole call: the wait for an earlier send to drain plus
// the wait for this one.  The device has a single slot and silently ignores a
// SEND rung while busy, so the payload is staged and rung only once the slot
// is free -- ringing early would lose it and still return nil.  A device
// still busy at the deadline is reported as an error wrapping
// os.ErrDeadlineExceeded instead of being waited on, so a stuck device cannot
// stall the caller.
//
// Send is safe for concurrent use; callers are serialised on the device, and a
// Close waits for an in-flight Send rather than pulling the mapping from it.
func (d *Device) Send(payload []byte, timeout time.Duration) error {
	if len(payload) == 0 {
		return fmt.Errorf("empty payload")
	}
	if len(payload) > maxPayload {
		return fmt.Errorf("payload of %d bytes exceeds the device ceiling of %d",
			len(payload), maxPayload)
	}
	deadline := time.Now().Add(timeout)

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.bar == nil || d.buf == nil {
		return fmt.Errorf("device is closed")
	}

	if !d.waitIdle(deadline) {
		return fmt.Errorf("device busy with an earlier send past the %v deadline, "+
			"payload not submitted: %w", timeout, os.ErrDeadlineExceeded)
	}

	copy(d.buf, payload)
	addr := uint64(uintptr(unsafe.Pointer(&d.buf[0])))

	d.wr(regDataPtrLo, uint32(addr))
	d.wr(regDataPtrHi, uint32(addr>>32))
	d.wr(regLength, uint32(len(payload)))
	d.wr(regCmd, cmdSend) // the trap: the device measures us before it keeps this

	// busy clears when the device is done, kept or dropped alike; it only
	// paces the next send.  Under KVM it is normally clear already; under TCG
	// it stays set for the length of the check.
	if !d.waitIdle(deadline) {
		return fmt.Errorf("device still busy with this send past the %v deadline: %w",
			timeout, os.ErrDeadlineExceeded)
	}
	return nil
}

// waitIdle polls busy until it clears, giving up once deadline has passed; it
// reports whether the device is idle.  The last sleep is cut to the deadline,
// so the wait does not overrun it by a poll interval.
func (d *Device) waitIdle(deadline time.Time) bool {
	for d.rd(regStatus)&stBusy != 0 {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		time.Sleep(min(busyPollInterval, left))
	}
	return true
}

/* ---------------------------------------------------------------- */
/* register access                                                   */
/* ---------------------------------------------------------------- */

// Atomic loads/stores keep the compiler from eliding or reordering these
// MMIO accesses; the registers are 32 bit and 4-byte aligned.
func (d *Device) rd(off uintptr) uint32 {
	return atomic.LoadUint32((*uint32)(unsafe.Pointer(&d.bar[off])))
}

func (d *Device) wr(off uintptr, v uint32) {
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&d.bar[off])), v)
}

/* ---------------------------------------------------------------- */
/* device discovery                                                  */
/* ---------------------------------------------------------------- */

func readHex(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 0, 32)
}

func findDevice() (string, error) {
	const root = "/sys/bus/pci/devices"
	ents, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("%s: %w", root, err)
	}
	for _, e := range ents {
		vendor, err := readHex(filepath.Join(root, e.Name(), "vendor"))
		if err != nil {
			continue
		}
		device, err := readHex(filepath.Join(root, e.Name(), "device"))
		if err != nil {
			continue
		}
		if vendor == pciVendor && device == pciDevice {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("%w (%04x:%04x) in this guest", ErrNoDevice, pciVendor, pciDevice)
}

// Nothing binds a driver to this device, so make sure its memory decoding is
// on before touching the BAR.  Firmware usually has done it already.
func enableMemoryDecode(slot string) {
	path := filepath.Join("/sys/bus/pci/devices", slot, "config")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	var b [2]byte
	if _, err := f.ReadAt(b[:], 4); err != nil {
		return
	}
	cmd := uint16(b[0]) | uint16(b[1])<<8
	if cmd&0x2 == 0 {
		cmd |= 0x2
		b[0], b[1] = byte(cmd), byte(cmd>>8)
		_, _ = f.WriteAt(b[:], 4) // best effort, like the rest of this function
	}
}
