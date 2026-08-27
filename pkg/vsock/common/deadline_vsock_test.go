//go:build linux

// This file is Linux-only. It drives a real AF_VSOCK loopback connection, and
// VMADDR_CID_LOCAL exists only in the Linux build of golang.org/x/sys/unix
// (darwin defines VMADDR_CID_ANY/_HOST but not _LOCAL), so without the tag the
// package fails to build on the macOS dev host even though the shipped binaries
// are linux/amd64 only. The rest of the suite stays cross-platform; run these
// with `make docker-test`.

package common

import (
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The read-deadline path rests on one assumption that the socketpair-based
// tests cannot check: that AF_VSOCK actually honours SO_RCVTIMEO. It does —
// SO_RCVTIMEO is a generic SOL_SOCKET option handled by sock_setsockopt()
// (net/core/sock.c), and vsock_connectible_recvmsg() in net/vmw_vsock/af_vsock.c
// feeds sock_rcvtimeo() into its wait, returning -EAGAIN when it expires — but
// an assumption stated in a comment is not an assumption under test. These
// tests exercise it against a real vsock connection over the loopback CID.
//
// They need the vsock_loopback kernel module, so they skip rather than fail
// where AF_VSOCK or VMADDR_CID_LOCAL is unavailable.

// newVsockLoopbackConns returns a connected client/server VsockConn pair over
// AF_VSOCK loopback, reproducing the receiver's setup: the listener is
// non-blocking exactly as server.NewVsockListener makes it, so the accepted fd
// carries whatever blocking mode accept() really gives it.
func newVsockLoopbackConns(t *testing.T, port uint32) (client, server *VsockConn) {
	t.Helper()

	lfd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("AF_VSOCK unavailable: %v", err)
	}
	closeListener := func() { _ = unix.Close(lfd) }

	if err := unix.SetNonblock(lfd, true); err != nil {
		closeListener()
		t.Fatalf("unix.SetNonblock: %v", err)
	}
	if err := unix.Bind(lfd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		closeListener()
		t.Skipf("cannot bind AF_VSOCK port %d: %v", port, err)
	}
	if err := unix.Listen(lfd, 1); err != nil {
		closeListener()
		t.Skipf("cannot listen on AF_VSOCK: %v", err)
	}

	cfd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		closeListener()
		t.Skipf("AF_VSOCK client socket: %v", err)
	}
	if err := unix.Connect(cfd, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: port}); err != nil {
		closeListener()
		_ = unix.Close(cfd)
		t.Skipf("no AF_VSOCK loopback (vsock_loopback module missing?): %v", err)
	}

	var afd int
	deadline := time.Now().Add(2 * time.Second)
	for {
		afd, _, err = unix.Accept(lfd)
		if err == nil {
			break
		}
		if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			closeListener()
			_ = unix.Close(cfd)
			t.Fatalf("unix.Accept: %v", err)
		}
		if time.Now().After(deadline) {
			closeListener()
			_ = unix.Close(cfd)
			t.Fatal("timed out accepting the loopback vsock connection")
		}
		time.Sleep(5 * time.Millisecond)
	}

	closeListener()
	t.Cleanup(func() {
		_ = unix.Close(cfd)
		_ = unix.Close(afd)
	})
	return NewVsockConn(cfd), NewVsockConn(afd)
}

// TestVsockAcceptedConnIsBlocking pins down why the deadline logic is safe to
// arm on every read. server.NewVsockListener marks the listen fd O_NONBLOCK; if
// accept() propagated that to the accepted fd, unix.Read would return EAGAIN
// immediately and Read would translate it into a spurious
// os.ErrDeadlineExceeded — dropping every guest connection on its first read.
// Linux does not propagate it, and this test fails loudly if that ever changes.
func TestVsockAcceptedConnIsBlocking(t *testing.T) {
	_, server := newVsockLoopbackConns(t, 54331)

	flags, err := unix.FcntlInt(uintptr(server.fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("F_GETFL on accepted fd: %v", err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Fatal("accepted vsock fd inherited O_NONBLOCK from the listener: " +
			"reads would return EAGAIN instantly and be misreported as deadline expiry")
	}
}

// TestVsockReadDeadlineEnforced is the end-to-end proof over real AF_VSOCK: a
// silent peer must surface as os.ErrDeadlineExceeded, and only after the
// deadline has actually elapsed. Returning early would mean the error came from
// a non-blocking socket rather than an enforced timeout.
func TestVsockReadDeadlineEnforced(t *testing.T) {
	_, server := newVsockLoopbackConns(t, 54332)

	const timeout = 300 * time.Millisecond
	_ = server.SetReadDeadline(time.Now().Add(timeout))

	start := time.Now()
	_, err := server.RecvData()
	elapsed := time.Since(start)

	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("RecvData from a silent vsock peer = %v, want os.ErrDeadlineExceeded", err)
	}
	// Allow generous slack below the nominal timeout for timer granularity,
	// but an immediate return means SO_RCVTIMEO was not what stopped the read.
	if elapsed < timeout/2 {
		t.Fatalf("returned after %v, well before the %v deadline: "+
			"SO_RCVTIMEO does not appear to be enforcing the wait", elapsed, timeout)
	}
}

// TestVsockReadDeadlineAllowsNormalTraffic guards the other direction: arming a
// deadline must not disturb a healthy exporter whose frames arrive in time.
func TestVsockReadDeadlineAllowsNormalTraffic(t *testing.T) {
	client, server := newVsockLoopbackConns(t, 54333)

	payload := []byte("gpu metrics frame")
	go func() {
		_ = client.SendData(payload)
	}()

	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := server.RecvData()
	if err != nil {
		t.Fatalf("RecvData under an armed deadline: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

// TestVsockReadDeadlineCleared covers disarming: after the deadline is cleared
// the next Read must block for real traffic instead of inheriting the stale
// SO_RCVTIMEO and failing with a raw EAGAIN.
func TestVsockReadDeadlineCleared(t *testing.T) {
	client, server := newVsockLoopbackConns(t, 54334)

	// Arm, let it expire, then clear.
	_ = server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := server.RecvData(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("priming read = %v, want os.ErrDeadlineExceeded", err)
	}
	_ = server.SetReadDeadline(time.Time{})

	payload := []byte("after clearing")
	go func() {
		time.Sleep(250 * time.Millisecond) // longer than the deadline that just expired
		_ = client.SendData(payload)
	}()

	got, err := server.RecvData()
	if err != nil {
		t.Fatalf("RecvData after clearing the deadline: %v (stale SO_RCVTIMEO?)", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}
