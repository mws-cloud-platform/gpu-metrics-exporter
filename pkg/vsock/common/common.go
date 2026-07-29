package common

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"golang.org/x/sys/unix"
)

// VsockConn wraps a raw AF_VSOCK fd to implement net.Conn. Read/Write operate
// directly on the fd via unix.Read/unix.Write; Close is guarded so it is safe
// to call multiple times. VSOCK does not support real deadlines, so the
// Set*Deadline methods are no-ops.
type VsockConn struct {
	fd        int
	closeErr  error
	closeOnce sync.Once
}

// NewVsockConn wraps an already-connected AF_VSOCK file descriptor.
func NewVsockConn(fd int) *VsockConn {
	return &VsockConn{fd: fd}
}

// Read implements io.Reader over the raw fd. A raw read(2) reports end-of-stream
// as (0, nil); to honour the io.Reader contract (which forbids (0, nil) for a
// non-empty buffer and would otherwise make io.ReadFull/binary.Read spin
// forever) it is translated to io.EOF.
func (c *VsockConn) Read(b []byte) (int, error) {
	n, err := unix.Read(c.fd, b)
	// A raw read(2) returns (0, nil) at end-of-stream, but the io.Reader
	// contract forbids returning (0, nil) for a non-empty buffer: io.ReadFull
	// and binary.Read (used for both the frame header and the payload) would
	// spin forever on it when a peer closes mid-frame. Translate EOF explicitly.
	if n == 0 && err == nil && len(b) > 0 {
		return 0, io.EOF
	}
	return n, err
}

// Write implements io.Writer over the raw fd.
func (c *VsockConn) Write(b []byte) (int, error) {
	return unix.Write(c.fd, b)
}

// Close shuts down and closes the underlying fd. It is safe to call multiple
// times; only the first call performs the actual close.
func (c *VsockConn) Close() error {
	c.closeOnce.Do(func() {
		_ = unix.Shutdown(c.fd, unix.SHUT_RDWR)
		c.closeErr = unix.Close(c.fd)
	})
	return c.closeErr
}

// SetDeadline is a no-op: AF_VSOCK does not support deadlines natively.
func (c *VsockConn) SetDeadline(t time.Time) error {
	// VSOCK doesn't support deadlines natively
	// Could implement with goroutines + timers if needed
	return nil
}

// SetReadDeadline is a no-op: AF_VSOCK does not support deadlines natively.
func (c *VsockConn) SetReadDeadline(t time.Time) error {
	return nil
}

// SetWriteDeadline is a no-op: AF_VSOCK does not support deadlines natively.
func (c *VsockConn) SetWriteDeadline(t time.Time) error {
	return nil
}

// VsockAddr implements net.Addr for an AF_VSOCK endpoint, identified by a
// (CID, Port) pair.
type VsockAddr struct {
	CID  uint32
	Port uint32
}

// Network returns the address network name, "vsock".
func (a *VsockAddr) Network() string { return "vsock" }

// String returns the address formatted as "CID:Port".
func (a *VsockAddr) String() string { return fmt.Sprintf("%d:%d", a.CID, a.Port) }

// LocalAddr returns the local vsock address of the connection, or a zero
// VsockAddr if it cannot be determined.
func (c *VsockConn) LocalAddr() *VsockAddr {
	// Get the local address from the socket
	sa, err := unix.Getsockname(c.fd)
	if err != nil {
		return &VsockAddr{}
	}
	vmAddr, ok := sa.(*unix.SockaddrVM)
	if !ok {
		return &VsockAddr{}
	}
	return &VsockAddr{CID: vmAddr.CID, Port: vmAddr.Port}
}

// RemoteAddr returns the peer's vsock address, or a zero VsockAddr if it
// cannot be determined. The CID is used by the receiver to identify the
// originating VM.
func (c *VsockConn) RemoteAddr() *VsockAddr {
	// Get the remote address from the socket
	sa, err := unix.Getpeername(c.fd)
	if err != nil {
		return &VsockAddr{}
	}
	vmAddr, ok := sa.(*unix.SockaddrVM)
	if !ok {
		return &VsockAddr{}
	}
	return &VsockAddr{CID: vmAddr.CID, Port: vmAddr.Port}
}

// VsockFrameHeader is the per-message framing prefix on the vsock stream: a
// magic number to detect misalignment, the payload length, and an xxhash64 of
// the payload for integrity checking.
type VsockFrameHeader struct {
	Magic uint32
	Len   uint32
	Hash  uint64
}

const vsockFrameHeaderMagic = uint32(0xBEADBEAF)
const vsockFrameMaxDataLength = 64 * 1024

// SendData writes data as one framed vsock message: a VsockFrameHeader followed
// by the payload. A zero-length payload is a valid control frame used as the
// exporter's "I'm done, close" signal. It returns an error if data exceeds
// vsockFrameMaxDataLength or the write fails.
func (c *VsockConn) SendData(data []byte) error {

	if len(data) > vsockFrameMaxDataLength {
		return fmt.Errorf("too much data %d to send in vsock frame", len(data))
	}

	hdr := VsockFrameHeader{Magic: vsockFrameHeaderMagic, Len: uint32(len(data)), Hash: xxhash.Sum64(data)}

	// Serialize the header into a buffer first, then writeAll: a single
	// unix.Write on a SOCK_STREAM socket may legitimately return fewer bytes
	// than requested, so both the header and the payload must be written in a
	// loop until fully flushed.
	var hdrBuf bytes.Buffer
	if err := binary.Write(&hdrBuf, binary.BigEndian, &hdr); err != nil {
		return err
	}
	if err := c.writeAll(hdrBuf.Bytes()); err != nil {
		return err
	}

	return c.writeAll(data)
}

// writeAll writes the whole buffer, looping past short writes. A SOCK_STREAM
// write() may return a short count without an error when the send buffer can't
// accept the full payload; giving up there (as io.ErrShortWrite does) would
// drop large frames.
func (c *VsockConn) writeAll(data []byte) error {
	for len(data) > 0 {
		n, err := c.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return fmt.Errorf("vsock write returned %d bytes", n)
		}
		data = data[n:]
	}
	return nil
}

var (
	// ErrNoData is returned by RecvData for a zero-length frame, which the
	// exporter sends to signal a clean end-of-stream. The receiver treats it as
	// a normal connection close.
	ErrNoData = fmt.Errorf("no data in vsock frame")
)

// RecvData reads one framed vsock message and returns its payload. It verifies
// the header magic and the payload hash, and rejects oversized lengths. A
// zero-length frame returns ErrNoData. Any read error (including a peer that
// closes mid-frame) is returned to the caller.
func (c *VsockConn) RecvData() ([]byte, error) {
	hdr := VsockFrameHeader{}

	err := binary.Read(c, binary.BigEndian, &hdr)
	if err != nil {
		return nil, err
	}

	if hdr.Magic != vsockFrameHeaderMagic {
		return nil, fmt.Errorf("invalid vsock frame header magic")
	}

	if hdr.Len == 0 {
		return nil, ErrNoData
	}

	if hdr.Len > vsockFrameMaxDataLength {
		return nil, fmt.Errorf("too much data in vsock frame: %d", hdr.Len)
	}

	data := make([]byte, hdr.Len)
	// io.ReadFull keeps reading until the buffer is full: a single unix.Read
	// on a SOCK_STREAM socket may return fewer bytes than requested without it
	// being EOF, so a plain Read would reject multi-KB frames as
	// io.ErrUnexpectedEOF before they're fully received.
	if _, err := io.ReadFull(c, data); err != nil {
		return nil, err
	}

	if hdr.Hash != xxhash.Sum64(data) {
		return nil, fmt.Errorf("unexpected vsock frame data hash")
	}

	return data, nil
}
