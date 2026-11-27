package exporterupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/client"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"go.uber.org/zap"
)

const (
	MetadataTimeout = 30 * time.Second
	DownloadTimeout = 10 * time.Minute
	ioTimeout       = 30 * time.Second
)

// Client is the guest side of the update protocol on the existing metrics port.
type Client struct {
	port int
	log  *zap.Logger
	dial func() (*common.VsockConn, error)
}

func NewClient(log *zap.Logger, port int) *Client {
	if log == nil {
		log = zap.NewNop()
	}
	c := &Client{port: port, log: log}
	c.dial = func() (*common.VsockConn, error) {
		return client.NewClientConnection(c.log, c.port)
	}
	return c
}

// DialFunc supplies a connected stream for one update session.
type DialFunc func() (*common.VsockConn, error)

// NewClientWithDial constructs a client with an explicit connection factory.
// It keeps protocol tests independent of AF_VSOCK availability.
func NewClientWithDial(dial DialFunc) *Client {
	return &Client{dial: dial}
}

func (c *Client) GetMetadata(ctx context.Context) (Metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, MetadataTimeout)
	defer cancel()
	conn, done, err := c.connect(ctx)
	if err != nil {
		return Metadata{}, err
	}
	defer done()
	resp, err := c.roundTrip(ctx, conn, Request{Command: CommandGetVersion})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Metadata{}, ctxErr
		}
		return Metadata{}, fmt.Errorf("query host exporter version: %w", err)
	}
	if resp.Offset != 0 || resp.EOF {
		return Metadata{}, fmt.Errorf("invalid metadata response range")
	}
	meta, err := decodeMetadata(resp.Payload)
	if err != nil {
		return Metadata{}, fmt.Errorf("decode host exporter metadata: %w", err)
	}
	return meta, nil
}

// Download writes exactly metadata.Size bytes to dst and verifies the digest of
// the complete file in addition to the common framing hash of every chunk.
func (c *Client) Download(ctx context.Context, metadata Metadata, dst io.Writer) error {
	if err := validateMetadata(metadata); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()
	conn, done, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer done()

	h := sha256.New()
	var offset int64
	for offset < metadata.Size {
		remaining := metadata.Size - offset
		length := int64(MaxChunkSize)
		if length > remaining {
			length = remaining
		}
		resp, err := c.roundTrip(ctx, conn, Request{
			Command: CommandRead,
			Offset:  uint64(offset),
			Length:  uint32(length),
		})
		if err != nil {
			return downloadError(ctx, err)
		}
		if resp.Offset != uint64(offset) {
			return fmt.Errorf("host returned offset %d, requested %d", resp.Offset, offset)
		}
		if len(resp.Payload) == 0 || len(resp.Payload) > int(length) || int64(len(resp.Payload)) > remaining {
			return fmt.Errorf("host returned invalid chunk length %d at offset %d (requested %d)", len(resp.Payload), offset, length)
		}
		if resp.EOF && int64(len(resp.Payload)) != remaining {
			return fmt.Errorf("host reported EOF after %d bytes with %d bytes still missing", offset+int64(len(resp.Payload)), remaining-int64(len(resp.Payload)))
		}
		n, err := dst.Write(resp.Payload)
		if err != nil {
			return err
		}
		if n != len(resp.Payload) {
			return io.ErrShortWrite
		}
		_, _ = h.Write(resp.Payload)
		offset += int64(n)
		if offset == metadata.Size && !resp.EOF {
			return fmt.Errorf("host did not mark the final chunk as EOF")
		}
	}
	if !bytes.Equal(h.Sum(nil), metadata.SHA256[:]) {
		return fmt.Errorf("downloaded package sha256 %x does not match host metadata %x", h.Sum(nil), metadata.SHA256)
	}
	return nil
}

func (c *Client) connect(ctx context.Context) (*common.VsockConn, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	conn, err := c.dial()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to host update service: %w", err)
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	done := func() {
		close(stop)
		_ = conn.Close()
	}
	return conn, done, nil
}

func (c *Client) roundTrip(ctx context.Context, conn *common.VsockConn, req Request) (Response, error) {
	if err := setOperationDeadline(ctx, conn); err != nil {
		return Response{}, err
	}
	if err := WriteRequest(conn, req); err != nil {
		return Response{}, fmt.Errorf("write update request: %w", err)
	}
	if err := setOperationDeadline(ctx, conn); err != nil {
		return Response{}, err
	}
	resp, err := ReadResponse(conn)
	if err != nil {
		return Response{}, err
	}
	if resp.Command != req.Command {
		return Response{}, fmt.Errorf("host answered command %d, expected %d", resp.Command, req.Command)
	}
	if resp.Status != StatusOK {
		return Response{}, &RemoteError{Status: resp.Status, Message: string(resp.Payload)}
	}
	return resp, nil
}

func setOperationDeadline(ctx context.Context, conn *common.VsockConn) error {
	deadline := time.Now().Add(ioTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	return conn.SetDeadline(deadline)
}

func downloadError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("download exporter package: %w", err)
}
