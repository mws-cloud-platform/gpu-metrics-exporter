package client

import (
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// vsockSendTimeoutSec bounds how long a single send can block. Without it, a
// host receiver that stops reading (stuck consumer, slow transport) would park
// the exporter's send goroutine forever, filling the metrics queue and preventing
// the query loop from observing its stop signal during shutdown. Generous
// relative to the tick period so legitimate slow sends still succeed.
const vsockSendTimeoutSec = 30

// NewClientConnection opens a fresh AF_VSOCK SOCK_STREAM connection from the
// guest to the host (CID 2) on the given port, with a bounded send timeout so a
// stalled host receiver surfaces as an error instead of hanging the exporter's
// send goroutine. The caller owns the returned connection and must Close it.
func NewClientConnection(log *zap.Logger, port int) (*common.VsockConn, error) {
	// Create AF_VSOCK socket
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		log.Error("unix.Socket", zap.Error(err))
		return nil, err
	}

	// Bound blocking writes: a timed-out write returns EAGAIN, which sendMetrics
	// surfaces as an error instead of hanging indefinitely.
	tv := unix.NsecToTimeval(int64(vsockSendTimeoutSec) * int64(time.Second))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv); err != nil {
		log.Warn("SetsockoptTimeval SO_SNDTIMEO", zap.Error(err))
	}

	// Connect to host (CID=2, port=1234)
	sa := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_HOST, // Host is always CID 2
		Port: uint32(port),
	}
	if err := unix.Connect(fd, sa); err != nil {
		log.Error("unix.Connect", zap.Error(err))
		_ = unix.Close(fd)
		return nil, err
	}

	con := common.NewVsockConn(fd)
	return con, nil
}
