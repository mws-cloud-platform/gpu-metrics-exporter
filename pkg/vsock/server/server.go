package server

import (
	"context"
	"net"
	"sync"

	vsockcommon "go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// VsockListener wraps a raw AF_VSOCK socket to implement net.Listener
type VsockListener struct {
	fd        int
	port      int
	log       *zap.Logger
	closeErr  error
	closeOnce sync.Once
}

const acceptPollTimeoutMs = 100

// Accept blocks until a new connection arrives or ctx is cancelled. It polls
// the non-blocking listen fd with a short timeout so that ctx.Done() or Close
// can interrupt an otherwise blocking accept.
func (l *VsockListener) Accept(ctx context.Context) (*vsockcommon.VsockConn, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		nfd, _, err := unix.Accept(l.fd)
		if err == nil {
			return vsockcommon.NewVsockConn(nfd), nil
		}
		if err == unix.EINTR {
			continue
		}
		if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			l.log.Warn("unix.Accept", zap.Error(err))
			return nil, err
		}

		pollFd := []unix.PollFd{{
			Fd:     int32(l.fd),
			Events: unix.POLLIN,
		}}

		n, err := unix.Poll(pollFd, acceptPollTimeoutMs)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			l.log.Warn("unix.Poll", zap.Error(err))
			return nil, err
		}
		if n == 0 {
			continue
		}
		if pollFd[0].Revents&unix.POLLNVAL != 0 {
			return nil, unix.EBADF
		}
	}
}

// Close shuts down and closes the listen fd. It is safe to call multiple times.
func (l *VsockListener) Close() error {
	l.closeOnce.Do(func() {
		_ = unix.Shutdown(l.fd, unix.SHUT_RDWR)
		l.closeErr = unix.Close(l.fd)
	})
	return l.closeErr
}

// Addr returns the listener's local vsock address.
func (l *VsockListener) Addr() net.Addr {
	return &vsockcommon.VsockAddr{CID: unix.VMADDR_CID_HOST, Port: uint32(l.port)}
}

const (
	vsockListenBacklog = 5
)

// NewVsockListener creates a non-blocking AF_VSOCK SOCK_STREAM listener bound
// to VMADDR_CID_ANY on the given port. Use Accept to await connections.
func NewVsockListener(log *zap.Logger, port int) (*VsockListener, error) {
	// Create AF_VSOCK socket
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		log.Error("unix.Socket", zap.Error(err))
		return nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		log.Error("unix.SetNonblock", zap.Error(err))
		unix.Close(fd)
		return nil, err
	}

	// Bind to host (CID=VMADDR_CID_ANY means host, port=1234)
	sa := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY, // Host
		Port: uint32(port),
	}
	if err := unix.Bind(fd, sa); err != nil {
		log.Error("unix.Bind", zap.Error(err))
		unix.Close(fd)
		return nil, err
	}

	// Listen
	if err := unix.Listen(fd, vsockListenBacklog); err != nil {
		log.Error("unix.Listen", zap.Error(err))
		unix.Close(fd)
		return nil, err
	}

	listener := &VsockListener{fd: fd, port: port, log: log}
	log.Info("host listening on vsock port", zap.Int("port", port))
	return listener, nil
}
