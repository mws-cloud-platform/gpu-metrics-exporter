package exporterupdate

import (
	"encoding/binary"
	"fmt"

	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
)

const (
	protocolVersion = uint16(1)
	requestMagic    = uint32(0x474d4555) // GMEU
	responseMagic   = uint32(0x474d4552) // GMER

	requestHeaderSize  = 24
	responseHeaderSize = 28
	maxResponsePayload = 64*1024 - responseHeaderSize

	// MaxChunkSize leaves ample room for the response header inside the common
	// 64 KiB vsock frame while keeping package downloads reasonably efficient.
	MaxChunkSize = 60 * 1024
	// MaxPackageSize is far above the exporter package's normal size of a few MiB.
	MaxPackageSize = int64(64 << 20)
	MaxVersionSize = 256
	maxErrorSize   = 4 * 1024

	responseFlagEOF = uint16(1)
)

// Command identifies one exporter update request.
type Command uint16

const (
	CommandGetVersion Command = 1
	CommandRead       Command = 2
)

// Status is the result of an update request.
type Status uint16

const (
	StatusOK Status = iota
	StatusBadRequest
	StatusUnavailable
	StatusInternal
)

// Request asks for the served package metadata or a byte range. Length is a
// maximum for Read: the server clamps it to MaxChunkSize and the remaining file.
type Request struct {
	Command Command
	Offset  uint64
	Length  uint32
}

// Response carries either metadata, raw package bytes, or a bounded error.
type Response struct {
	Command Command
	Status  Status
	Offset  uint64
	EOF     bool
	Payload []byte
}

// Metadata describes one immutable package snapshot.
type Metadata struct {
	Version string
	Size    int64
	SHA256  [32]byte
}

// RemoteError is an error response returned by the host.
type RemoteError struct {
	Status  Status
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("host update error (status %d): %s", e.Status, e.Message)
}

// IsRequestFrame distinguishes an update connection from a gzip-compressed
// metrics payload without changing the existing metrics wire format.
func IsRequestFrame(data []byte) bool {
	return len(data) >= 4 && binary.BigEndian.Uint32(data[:4]) == requestMagic
}

func WriteRequest(conn *common.VsockConn, req Request) error {
	buf := make([]byte, requestHeaderSize)
	binary.BigEndian.PutUint32(buf[0:4], requestMagic)
	binary.BigEndian.PutUint16(buf[4:6], protocolVersion)
	binary.BigEndian.PutUint16(buf[6:8], uint16(req.Command))
	binary.BigEndian.PutUint64(buf[8:16], req.Offset)
	binary.BigEndian.PutUint32(buf[16:20], req.Length)
	return conn.SendData(buf)
}

func ParseRequest(data []byte) (Request, error) {
	if len(data) != requestHeaderSize {
		return Request{}, fmt.Errorf("update request is %d bytes, want %d", len(data), requestHeaderSize)
	}
	if binary.BigEndian.Uint32(data[0:4]) != requestMagic {
		return Request{}, fmt.Errorf("invalid update request magic")
	}
	if got := binary.BigEndian.Uint16(data[4:6]); got != protocolVersion {
		return Request{}, fmt.Errorf("unsupported update protocol version %d", got)
	}
	if binary.BigEndian.Uint32(data[20:24]) != 0 {
		return Request{}, fmt.Errorf("update request reserved field is not zero")
	}
	req := Request{
		Command: Command(binary.BigEndian.Uint16(data[6:8])),
		Offset:  binary.BigEndian.Uint64(data[8:16]),
		Length:  binary.BigEndian.Uint32(data[16:20]),
	}
	switch req.Command {
	case CommandGetVersion:
		if req.Offset != 0 || req.Length != 0 {
			return Request{}, fmt.Errorf("get-version request has a range")
		}
	case CommandRead:
		if req.Length == 0 {
			return Request{}, fmt.Errorf("read request has zero length")
		}
	default:
		return Request{}, fmt.Errorf("unknown update command %d", req.Command)
	}
	return req, nil
}

func WriteResponse(conn *common.VsockConn, resp Response) error {
	if len(resp.Payload) > maxResponsePayload {
		return fmt.Errorf("update response payload is too large: %d", len(resp.Payload))
	}
	if resp.Status != StatusOK && len(resp.Payload) > maxErrorSize {
		return fmt.Errorf("update error response is too large: %d", len(resp.Payload))
	}
	buf := make([]byte, responseHeaderSize)
	binary.BigEndian.PutUint32(buf[0:4], responseMagic)
	binary.BigEndian.PutUint16(buf[4:6], protocolVersion)
	binary.BigEndian.PutUint16(buf[6:8], uint16(resp.Status))
	binary.BigEndian.PutUint16(buf[8:10], uint16(resp.Command))
	if resp.EOF {
		binary.BigEndian.PutUint16(buf[10:12], responseFlagEOF)
	}
	binary.BigEndian.PutUint64(buf[12:20], resp.Offset)
	binary.BigEndian.PutUint32(buf[20:24], uint32(len(resp.Payload)))
	return conn.SendData(append(buf, resp.Payload...))
}

func ReadResponse(conn *common.VsockConn) (Response, error) {
	data, err := conn.RecvData()
	if err != nil {
		return Response{}, fmt.Errorf("read update response: %w", err)
	}
	if len(data) < responseHeaderSize {
		return Response{}, fmt.Errorf("update response is %d bytes, shorter than its header", len(data))
	}
	if binary.BigEndian.Uint32(data[0:4]) != responseMagic {
		return Response{}, fmt.Errorf("invalid update response magic")
	}
	if got := binary.BigEndian.Uint16(data[4:6]); got != protocolVersion {
		return Response{}, fmt.Errorf("unsupported update response protocol %d", got)
	}
	flags := binary.BigEndian.Uint16(data[10:12])
	if flags&^responseFlagEOF != 0 {
		return Response{}, fmt.Errorf("update response has unknown flags %#x", flags)
	}
	if binary.BigEndian.Uint32(data[24:28]) != 0 {
		return Response{}, fmt.Errorf("update response reserved field is not zero")
	}
	length := int(binary.BigEndian.Uint32(data[20:24]))
	if length > maxResponsePayload || len(data) != responseHeaderSize+length {
		return Response{}, fmt.Errorf("update response length %d does not match frame size %d", length, len(data))
	}
	resp := Response{
		Status:  Status(binary.BigEndian.Uint16(data[6:8])),
		Command: Command(binary.BigEndian.Uint16(data[8:10])),
		EOF:     flags&responseFlagEOF != 0,
		Offset:  binary.BigEndian.Uint64(data[12:20]),
		Payload: data[responseHeaderSize:],
	}
	switch resp.Status {
	case StatusOK, StatusBadRequest, StatusUnavailable, StatusInternal:
	default:
		return Response{}, fmt.Errorf("unknown update response status %d", resp.Status)
	}
	if resp.Status != StatusOK && len(resp.Payload) > maxErrorSize {
		return Response{}, fmt.Errorf("update error response is too large: %d", len(resp.Payload))
	}
	return resp, nil
}

func encodeMetadata(meta Metadata) ([]byte, error) {
	if err := validateMetadata(meta); err != nil {
		return nil, err
	}
	buf := make([]byte, 44+len(meta.Version))
	binary.BigEndian.PutUint64(buf[0:8], uint64(meta.Size))
	copy(buf[8:40], meta.SHA256[:])
	binary.BigEndian.PutUint16(buf[40:42], uint16(len(meta.Version)))
	copy(buf[44:], meta.Version)
	return buf, nil
}

func decodeMetadata(data []byte) (Metadata, error) {
	if len(data) < 44 {
		return Metadata{}, fmt.Errorf("metadata response is too short: %d", len(data))
	}
	if binary.BigEndian.Uint16(data[42:44]) != 0 {
		return Metadata{}, fmt.Errorf("metadata reserved field is not zero")
	}
	versionSize := int(binary.BigEndian.Uint16(data[40:42]))
	if versionSize == 0 || versionSize > MaxVersionSize || len(data) != 44+versionSize {
		return Metadata{}, fmt.Errorf("invalid metadata version length %d for payload %d", versionSize, len(data))
	}
	size := binary.BigEndian.Uint64(data[0:8])
	if size > uint64(MaxPackageSize) {
		return Metadata{}, fmt.Errorf("package size %d exceeds %d", size, MaxPackageSize)
	}
	meta := Metadata{Version: string(data[44:]), Size: int64(size)}
	copy(meta.SHA256[:], data[8:40])
	if err := validateMetadata(meta); err != nil {
		return Metadata{}, err
	}
	return meta, nil
}

func validateMetadata(meta Metadata) error {
	if meta.Version == "" || len(meta.Version) > MaxVersionSize {
		return fmt.Errorf("invalid package version length %d", len(meta.Version))
	}
	if meta.Size <= 0 || meta.Size > MaxPackageSize {
		return fmt.Errorf("invalid package size %d", meta.Size)
	}
	return nil
}
