package common

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/cespare/xxhash/v2"
)

// newPipeConns returns a sender/receiver VsockConn pair backed by an os.Pipe.
// VsockConn speaks raw fd I/O (unix.Read/unix.Write), so a pipe exercises the
// real framing code path without needing AF_VSOCK. LocalAddr/RemoteAddr will
// return zero CIDs on a pipe (Getsockname/Getpeername fail on non-sockets),
// which is fine for framing-only tests.
func newPipeConns(t *testing.T) (sender, receiver *VsockConn) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	// Closing the os.File closes the underlying fd; VsockConn.Close would also
	// unix.Close the same fd. To avoid noisy double-closes we let VsockConn
	// tests that call Close own the fd, and only close via os.File otherwise.
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return NewVsockConn(int(w.Fd())), NewVsockConn(int(r.Fd()))
}

func TestSendRecvDataRoundTrip(t *testing.T) {
	sender, receiver := newPipeConns(t)

	payload := []byte("hello vsock framing")
	if err := sender.SendData(payload); err != nil {
		t.Fatalf("SendData: %v", err)
	}

	got, err := receiver.RecvData()
	if err != nil {
		t.Fatalf("RecvData: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %q want %q", got, payload)
	}
}

// TestSendRecvLargeFrame exercises writeAll / io.ReadFull across short writes
// and reads: a near-max-size payload won't fit in a single pipe write, so both
// sides must loop to flush/reassemble the whole frame. The receiver runs in a
// goroutine because an os.Pipe buffers only ~16 KiB — a blocking 64 KiB write
// with nobody reading on the other end would deadlock.
func TestSendRecvLargeFrame(t *testing.T) {
	sender, receiver := newPipeConns(t)

	payload := make([]byte, vsockFrameMaxDataLength-1)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		d, err := receiver.RecvData()
		ch <- result{d, err}
	}()

	if err := sender.SendData(payload); err != nil {
		t.Fatalf("SendData: %v", err)
	}
	res := <-ch
	if res.err != nil {
		t.Fatalf("RecvData: %v", res.err)
	}
	if !bytes.Equal(res.data, payload) {
		t.Fatalf("large frame mismatch: len got %d want %d", len(res.data), len(payload))
	}
}

// TestEmptyFrameReturnsErrNoData pins the close-connection contract: a Len=0
// frame is the exporter's "I'm done" signal, which the receiver treats as a
// clean end-of-stream (common.ErrNoData).
func TestEmptyFrameReturnsErrNoData(t *testing.T) {
	sender, receiver := newPipeConns(t)

	if err := sender.SendData([]byte{}); err != nil {
		t.Fatalf("SendData(empty): %v", err)
	}
	_, err := receiver.RecvData()
	if !errors.Is(err, ErrNoData) {
		t.Fatalf("expected ErrNoData, got %v", err)
	}
}

func TestSendDataTooLarge(t *testing.T) {
	sender, _ := newPipeConns(t)

	payload := make([]byte, vsockFrameMaxDataLength+1)
	err := sender.SendData(payload)
	if err == nil {
		t.Fatal("expected error for oversized payload, got nil")
	}
}

// TestRecvDataInvalidMagic feeds a frame with a wrong magic and expects the
// receiver to reject it rather than decoding garbage.
func TestRecvDataInvalidMagic(t *testing.T) {
	sender, receiver := newPipeConns(t)

	var buf bytes.Buffer
	hdr := VsockFrameHeader{Magic: 0xDEADBEEF, Len: 4, Hash: xxhash.Sum64([]byte("test"))}
	if err := binary.Write(&buf, binary.BigEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("test")
	if _, err := sender.Write(buf.Bytes()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, err := receiver.RecvData()
	if err == nil {
		t.Fatal("expected invalid-magic error, got nil")
	}
}

// TestRecvDataHashMismatch feeds a well-formed header (right magic/len) whose
// stored hash doesn't match the payload, and expects rejection.
func TestRecvDataHashMismatch(t *testing.T) {
	sender, receiver := newPipeConns(t)

	payload := []byte("tampered")
	var buf bytes.Buffer
	hdr := VsockFrameHeader{
		Magic: vsockFrameHeaderMagic,
		Len:   uint32(len(payload)),
		Hash:  xxhash.Sum64([]byte("original")), // wrong hash on purpose
	}
	if err := binary.Write(&buf, binary.BigEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	buf.Write(payload)
	if _, err := sender.Write(buf.Bytes()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, err := receiver.RecvData()
	if err == nil {
		t.Fatal("expected hash-mismatch error, got nil")
	}
}

// TestRecvDataLenExceedsMax feeds a header claiming more than the max payload
// and expects the server-side size guard to fire.
func TestRecvDataLenExceedsMax(t *testing.T) {
	sender, receiver := newPipeConns(t)

	var buf bytes.Buffer
	hdr := VsockFrameHeader{
		Magic: vsockFrameHeaderMagic,
		Len:   vsockFrameMaxDataLength + 1,
		Hash:  0,
	}
	if err := binary.Write(&buf, binary.BigEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Write(buf.Bytes()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, err := receiver.RecvData()
	if err == nil {
		t.Fatal("expected too-much-data error, got nil")
	}
}

// TestRecvDataPeerClosedMidFrame sends only the header (no payload) and closes
// the write end. Before VsockConn.Read translated raw read(2)'s (0,nil) EOF into
// io.EOF, io.ReadFull spun forever here; now RecvData must surface a prompt
// error instead of hanging.
func TestRecvDataPeerClosedMidFrame(t *testing.T) {
	sender, receiver := newPipeConns(t)

	payload := []byte("only header, no body follows")
	var buf bytes.Buffer
	hdr := VsockFrameHeader{
		Magic: vsockFrameHeaderMagic,
		Len:   uint32(len(payload)),
		Hash:  xxhash.Sum64(payload),
	}
	if err := binary.Write(&buf, binary.BigEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Write(buf.Bytes()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Close the write side so the reader hits EOF before the payload arrives.
	if err := sender.Close(); err != nil {
		t.Fatalf("sender.Close: %v", err)
	}

	done := make(chan error, 1)
	go func() { _, err := receiver.RecvData(); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for peer-closed-mid-frame, got nil")
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			t.Logf("mid-frame close error (acceptable): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RecvData hung on peer-closed-mid-frame; Read did not translate EOF")
	}
}

// TestRecvDataPeerClosedBeforeHeader closes the write end without sending
// anything. binary.Read on the header must fail promptly, not hang.
func TestRecvDataPeerClosedBeforeHeader(t *testing.T) {
	sender, receiver := newPipeConns(t)

	if err := sender.Close(); err != nil {
		t.Fatalf("sender.Close: %v", err)
	}

	done := make(chan error, 1)
	go func() { _, err := receiver.RecvData(); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error for closed-before-header, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RecvData hung on closed-before-header; Read did not translate EOF")
	}
}

// TestMultipleFramesInStream ensures back-to-back frames are delimited
// correctly on a stream socket (no framing bleed-over).
func TestMultipleFramesInStream(t *testing.T) {
	sender, receiver := newPipeConns(t)

	frames := [][]byte{
		[]byte("first"),
		[]byte("second frame is a bit longer"),
		[]byte("3"),
	}
	for _, f := range frames {
		if err := sender.SendData(f); err != nil {
			t.Fatalf("SendData: %v", err)
		}
	}

	for i, want := range frames {
		got, err := receiver.RecvData()
		if err != nil {
			t.Fatalf("frame %d RecvData: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d mismatch: got %q want %q", i, got, want)
		}
	}
}

// TestCloseIdempotent verifies closeOnce: a second Close must not panic or
// double-close the fd in a way that surfaces a non-nil error crash.
func TestCloseIdempotent(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	conn := NewVsockConn(int(w.Fd()))

	if err := conn.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must be a safe no-op (closeOnce guards the real work).
	if err := conn.Close(); err != nil {
		// A second unix.Close would normally EBADF; closeOnce prevents that, so
		// we expect the cached (nil) error from the first call.
		t.Fatalf("second Close returned error: %v", err)
	}
	_ = r.Close()
}

func TestVsockAddrString(t *testing.T) {
	a := &VsockAddr{CID: 7, Port: 1234}
	if got, want := a.String(), "7:1234"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if a.Network() != "vsock" {
		t.Fatalf("Network = %q, want %q", a.Network(), "vsock")
	}
}
