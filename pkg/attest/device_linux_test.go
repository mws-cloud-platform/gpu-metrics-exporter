package attest

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// idleV1 is STATUS of an idle device speaking protocol v1. Every fake device
// starts with it, so the tests also pin that busy-polling looks at the busy
// bit alone, not at a register whose high half is never zero.
const idleV1 = protoVersion << stVersionShift

// newFakeDevice returns a Device whose BAR is plain anonymous memory, so the
// doorbell sequence can be driven and inspected without the QEMU device.
// Nothing in it sets or clears busy except the test itself.
func newFakeDevice(t *testing.T) *Device {
	t.Helper()
	bar, err := syscall.Mmap(-1, 0, barSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		t.Fatalf("mmap fake BAR: %v", err)
	}
	buf, err := syscall.Mmap(-1, 0, maxPayload,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		_ = syscall.Munmap(bar)
		t.Fatalf("mmap payload buffer: %v", err)
	}
	d := &Device{bar: bar, buf: buf}
	d.wr(regStatus, idleV1)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestCheckProtocol pins the ABI check Open makes: STATUS[31:16] must carry
// the protocol version this package speaks, whatever busy says -- a device
// caught mid-send at open still matches -- and anything else is refused,
// including the 0 a device from before the version field reads.
func TestCheckProtocol(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status uint32
		ok     bool
	}{
		{"v1 idle", idleV1, true},
		{"v1 busy", idleV1 | stBusy, true},
		{"v2", 2 << stVersionShift, false},
		{"no version field", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			d.wr(regStatus, tc.status)

			err := d.checkProtocol()
			if tc.ok && err != nil {
				t.Errorf("STATUS %#x refused: %v", tc.status, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("STATUS %#x accepted, want refused", tc.status)
			}
		})
	}
}

// TestSendRingsDoorbell pins the register sequence an idle device sees: the
// staged payload's address and length, then SEND.
func TestSendRingsDoorbell(t *testing.T) {
	d := newFakeDevice(t)
	payload := []byte("gzipped metrics")

	if err := d.Send(payload, time.Second); err != nil {
		t.Fatalf("Send: %v", err)
	}

	addr := uint64(uintptr(unsafe.Pointer(&d.buf[0])))
	for _, r := range []struct {
		name string
		off  uintptr
		want uint32
	}{
		{"DATA_PTR_LO", regDataPtrLo, uint32(addr)},
		{"DATA_PTR_HI", regDataPtrHi, uint32(addr >> 32)},
		{"LENGTH", regLength, uint32(len(payload))},
		{"CMD", regCmd, cmdSend},
	} {
		if got := d.rd(r.off); got != r.want {
			t.Errorf("%s = %#x, want %#x", r.name, got, r.want)
		}
	}
	if got := d.buf[:len(payload)]; !bytes.Equal(got, payload) {
		t.Errorf("staged payload = %q, want %q", got, payload)
	}
}

// TestSendGivesUpOnBusyDevice is the guard on the timeout: a device that never
// clears busy must cost the caller the timeout, not hang it -- the exporter
// sends inline, ahead of the vsock send. Nothing may be staged or rung either:
// the device ignores SEND while busy, and the buffer may still belong to the
// earlier send.
func TestSendGivesUpOnBusyDevice(t *testing.T) {
	d := newFakeDevice(t)
	d.wr(regStatus, idleV1|stBusy)

	const timeout = 50 * time.Millisecond
	start := time.Now()
	err := d.Send([]byte("payload"), timeout)
	elapsed := time.Since(start)

	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Send error = %v, want one wrapping os.ErrDeadlineExceeded", err)
	}
	if elapsed < timeout {
		t.Errorf("Send gave up after %v, before its %v timeout", elapsed, timeout)
	}
	// Generous: a loaded CI box can oversleep, but not by a second.
	if elapsed > timeout+time.Second {
		t.Errorf("Send took %v, far past its %v timeout", elapsed, timeout)
	}
	if got := d.rd(regCmd); got != 0 {
		t.Errorf("CMD = %d, want 0: SEND rung on a busy device", got)
	}
	if d.buf[0] != 0 {
		t.Errorf("payload staged into the buffer while the device was busy")
	}
}

// TestSendWaitsForEarlierSendToDrain covers the slot being freed within the
// timeout: the send waits for it, then goes out.
func TestSendWaitsForEarlierSendToDrain(t *testing.T) {
	d := newFakeDevice(t)
	d.wr(regStatus, idleV1|stBusy)

	const drain = 20 * time.Millisecond
	cleared := make(chan struct{})
	go func() {
		defer close(cleared)
		time.Sleep(drain)
		d.wr(regStatus, idleV1)
	}()

	start := time.Now()
	err := d.Send([]byte("payload"), 2*time.Second)
	elapsed := time.Since(start)
	<-cleared

	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if elapsed < drain {
		t.Errorf("Send returned after %v, before the earlier send drained (%v)", elapsed, drain)
	}
	if got := d.rd(regCmd); got != cmdSend {
		t.Errorf("CMD = %d, want %d once the device went idle", got, cmdSend)
	}
}

// TestSendRejectsLocally covers the errors Send raises without touching the
// device.
func TestSendRejectsLocally(t *testing.T) {
	d := newFakeDevice(t)

	if err := d.Send(nil, time.Second); err == nil {
		t.Error("Send(empty) = nil, want an error")
	}
	if err := d.Send(make([]byte, maxPayload+1), time.Second); err == nil {
		t.Error("Send(oversize) = nil, want an error")
	}
	if got := d.rd(regCmd); got != 0 {
		t.Errorf("CMD = %d after rejected sends, want 0", got)
	}

	_ = d.Close()
	if err := d.Send([]byte("payload"), time.Second); err == nil {
		t.Error("Send after Close = nil, want an error")
	}
}
