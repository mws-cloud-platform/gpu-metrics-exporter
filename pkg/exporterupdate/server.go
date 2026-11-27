package exporterupdate

import (
	"errors"
	"fmt"
	"io"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
)

// Server serves update commands after the receiver has read and classified the
// first frame. The receiver remains responsible for peer CID and connection
// limits; closing the connection interrupts Serve during receiver shutdown.
// Each connection also has request and range-byte budgets, so a guest cannot
// keep a slot forever by issuing an unbounded sequence of valid reads.
type Server struct {
	Package *Package

	// Zero values use defaults sized for one maximum-size package using client
	// chunks. Tests lower them to exercise the limits without large transfers.
	maxRequests  int
	maxReadBytes uint64
}

const (
	defaultMaxRequests  = 2048 // 1094 requests suffice for 64 MiB at 60 KiB plus metadata
	defaultMaxReadBytes = uint64(MaxPackageSize)
)

func (s *Server) Serve(conn *common.VsockConn, first []byte, readTimeout, writeTimeout time.Duration) error {
	requestLimit := s.maxRequests
	if requestLimit <= 0 {
		requestLimit = defaultMaxRequests
	}
	byteLimit := s.maxReadBytes
	if byteLimit == 0 {
		byteLimit = defaultMaxReadBytes
	}
	requests := 0
	var readBytes uint64
	data := first
	for {
		req, err := ParseRequest(data)
		if err != nil {
			resp := Response{Status: StatusBadRequest, Payload: errorPayload(err)}
			if writeErr := writeResponse(conn, resp, writeTimeout); writeErr != nil {
				return writeErr
			}
			if waitErr := waitForPeerClose(conn, readTimeout); waitErr != nil {
				return waitErr
			}
			return err
		}
		if requests >= requestLimit {
			if err := writeResponse(conn, workLimitResponse(req, "update request limit reached"), writeTimeout); err != nil {
				return err
			}
			return waitForPeerClose(conn, readTimeout)
		}
		requests++

		resp := s.respond(req)
		limitReached := false
		if req.Command == CommandRead && resp.Status == StatusOK {
			n := uint64(len(resp.Payload))
			// Written this way rather than readBytes+n > byteLimit to avoid
			// overflow even with test-specific limits.
			if n > byteLimit || readBytes > byteLimit-n {
				resp = workLimitResponse(req, "update range-byte limit reached")
				limitReached = true
			} else {
				readBytes += n
			}
		}
		if err := writeResponse(conn, resp, writeTimeout); err != nil {
			return err
		}
		if limitReached {
			return waitForPeerClose(conn, readTimeout)
		}

		if readTimeout > 0 {
			if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
				return err
			}
		}
		data, err = conn.RecvData()
		if err != nil {
			if errors.Is(err, common.ErrNoData) || errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read update request: %w", err)
		}
	}
}

func waitForPeerClose(conn *common.VsockConn, timeout time.Duration) error {
	// Closing immediately after a terminal response can race with the peer's read
	// on some stream transports. Wait for the client to close after it receives
	// the error, with one absolute deadline for a peer that keeps the fd open.
	if timeout > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
	}
	for {
		_, err := conn.RecvData()
		if errors.Is(err, common.ErrNoData) || errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func workLimitResponse(req Request, message string) Response {
	return Response{Command: req.Command, Status: StatusBadRequest, Offset: req.Offset, Payload: []byte(message)}
}

func (s *Server) respond(req Request) Response {
	resp := Response{Command: req.Command, Status: StatusOK, Offset: req.Offset}
	if s.Package == nil {
		resp.Status = StatusUnavailable
		resp.Payload = []byte("exporter update package is not configured on the host")
		return resp
	}
	meta := s.Package.Metadata()
	if req.Command == CommandGetVersion {
		payload, err := encodeMetadata(meta)
		if err != nil {
			resp.Status = StatusInternal
			resp.Payload = errorPayload(err)
			return resp
		}
		resp.Payload = payload
		return resp
	}

	if req.Offset > uint64(meta.Size) {
		resp.Status = StatusBadRequest
		resp.Payload = []byte(fmt.Sprintf("offset %d is beyond package size %d", req.Offset, meta.Size))
		return resp
	}
	remaining := uint64(meta.Size) - req.Offset
	length := uint64(req.Length)
	if length > MaxChunkSize {
		length = MaxChunkSize
	}
	if length > remaining {
		length = remaining
	}
	data := make([]byte, int(length))
	if length > 0 {
		n, err := s.Package.file.ReadAt(data, int64(req.Offset))
		if err != nil {
			resp.Status = StatusInternal
			resp.Payload = errorPayload(fmt.Errorf("read exporter package: %w", err))
			return resp
		}
		if n != len(data) {
			resp.Status = StatusInternal
			resp.Payload = []byte(fmt.Sprintf("short package read: got %d bytes, want %d", n, len(data)))
			return resp
		}
	}
	resp.Payload = data
	resp.EOF = req.Offset+length == uint64(meta.Size)
	return resp
}

func writeResponse(conn *common.VsockConn, resp Response, timeout time.Duration) error {
	if timeout > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
	}
	if err := WriteResponse(conn, resp); err != nil {
		return fmt.Errorf("write update response: %w", err)
	}
	return nil
}

func errorPayload(err error) []byte {
	msg := []byte(err.Error())
	if len(msg) > maxErrorSize {
		msg = msg[:maxErrorSize]
	}
	return msg
}
