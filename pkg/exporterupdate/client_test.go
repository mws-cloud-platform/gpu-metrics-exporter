package exporterupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"golang.org/x/sys/unix"
)

func updateSocketpair(t *testing.T) (*common.VsockConn, *common.VsockConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return common.NewVsockConn(fds[0]), common.NewVsockConn(fds[1])
}

func testPackage(t *testing.T, data []byte, version string) *Package {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exporter.deb")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := OpenPackage(path, version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pkg.Close() })
	return pkg
}

func startUpdateServer(t *testing.T, server *Server) (clientConn *common.VsockConn) {
	t.Helper()
	clientConn, serverConn := updateSocketpair(t)
	done := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		first, err := serverConn.RecvData()
		if err != nil {
			done <- err
			return
		}
		if !IsRequestFrame(first) {
			done <- fmt.Errorf("first frame is not an update request")
			return
		}
		done <- server.Serve(serverConn, first, 2*time.Second, 2*time.Second)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("update server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("update server did not stop")
		}
	})
	return clientConn
}

func testClient(t *testing.T, server *Server) *Client {
	t.Helper()
	c := &Client{}
	c.dial = func() (*common.VsockConn, error) {
		return startUpdateServer(t, server), nil
	}
	return c
}

func TestProtocolRoundTrip(t *testing.T) {
	sender, receiver := updateSocketpair(t)
	defer func() { _ = sender.Close() }()
	defer func() { _ = receiver.Close() }()
	want := Request{Command: CommandRead, Offset: 123, Length: 456}
	if err := WriteRequest(sender, want); err != nil {
		t.Fatal(err)
	}
	data, err := receiver.RecvData()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("request %+v, want %+v", got, want)
	}

	meta := Metadata{Version: "2026.11.24-1", Size: 1234, SHA256: sha256.Sum256([]byte("package"))}
	payload, err := encodeMetadata(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteResponse(sender, Response{Command: CommandGetVersion, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	resp, err := ReadResponse(receiver)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMetadata(resp.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Version != meta.Version || decoded.Size != meta.Size || decoded.SHA256 != meta.SHA256 {
		t.Fatalf("metadata %+v, want %+v", decoded, meta)
	}
}

func TestParseRequestRejectsMalformed(t *testing.T) {
	valid := make([]byte, requestHeaderSize)
	binaryBigEndianPutRequest(valid, Request{Command: CommandRead, Offset: 1, Length: 1})
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"short", func(b []byte) []byte { return b[:requestHeaderSize-1] }},
		{"magic", func(b []byte) []byte { b[0]++; return b }},
		{"version", func(b []byte) []byte { b[5]++; return b }},
		{"reserved", func(b []byte) []byte { b[20] = 1; return b }},
		{"unknown command", func(b []byte) []byte { b[7] = 99; return b }},
		{"zero read length", func(b []byte) []byte {
			binaryBigEndianPutRequest(b, Request{Command: CommandRead, Offset: 1})
			return b
		}},
		{"get version range", func(b []byte) []byte {
			binaryBigEndianPutRequest(b, Request{Command: CommandGetVersion, Length: 1})
			return b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.mutate(append([]byte(nil), valid...))
			if _, err := ParseRequest(data); err == nil {
				t.Fatal("ParseRequest accepted malformed input")
			}
		})
	}
}

func binaryBigEndianPutRequest(buf []byte, req Request) {
	for i := range buf {
		buf[i] = 0
	}
	put32 := func(at int, v uint32) {
		buf[at] = byte(v >> 24)
		buf[at+1] = byte(v >> 16)
		buf[at+2] = byte(v >> 8)
		buf[at+3] = byte(v)
	}
	put32(0, requestMagic)
	buf[4] = byte(protocolVersion >> 8)
	buf[5] = byte(protocolVersion)
	buf[6] = byte(req.Command >> 8)
	buf[7] = byte(req.Command)
	for i := 0; i < 8; i++ {
		buf[8+i] = byte(req.Offset >> (56 - 8*i))
	}
	put32(16, req.Length)
}

func TestClientDownloadsWholePackage(t *testing.T) {
	data := make([]byte, 2*MaxChunkSize+137)
	for i := range data {
		data[i] = byte(i % 251)
	}
	pkg := testPackage(t, data, "2026.11.24-1")
	c := testClient(t, &Server{Package: pkg})

	meta, err := c.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != "2026.11.24-1" || meta.Size != int64(len(data)) || meta.SHA256 != sha256.Sum256(data) {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	var downloaded bytes.Buffer
	if err := c.Download(context.Background(), meta, &downloaded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(downloaded.Bytes(), data) {
		t.Fatal("downloaded package differs from source")
	}
}

func TestServerReadRangesAndEOF(t *testing.T) {
	data := bytes.Repeat([]byte("abc"), (MaxChunkSize+300)/3+1)
	pkg := testPackage(t, data, "2026.11.24-1")
	conn := startUpdateServer(t, &Server{Package: pkg})
	ctx := context.Background()
	c := &Client{}

	metaResp, err := c.roundTrip(ctx, conn, Request{Command: CommandGetVersion})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := decodeMetadata(metaResp.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		offset     uint64
		length     uint32
		wantLength int
		wantEOF    bool
	}{
		{"middle", 3, 7, 7, false},
		{"last range truncated", uint64(len(data) - 5), 100, 5, true},
		{"chunk capped", 0, MaxChunkSize + 100, MaxChunkSize, false},
		{"exact EOF", uint64(len(data)), 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := c.roundTrip(ctx, conn, Request{Command: CommandRead, Offset: tc.offset, Length: tc.length})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Offset != tc.offset || len(resp.Payload) != tc.wantLength || resp.EOF != tc.wantEOF {
				t.Fatalf("response offset=%d len=%d eof=%v, want %d/%d/%v", resp.Offset, len(resp.Payload), resp.EOF, tc.offset, tc.wantLength, tc.wantEOF)
			}
			if len(resp.Payload) > 0 && !bytes.Equal(resp.Payload, data[tc.offset:int(tc.offset)+tc.wantLength]) {
				t.Fatal("range data mismatch")
			}
		})
	}
	resp, err := ReadResponseAfterRequest(conn, Request{Command: CommandRead, Offset: uint64(meta.Size) + 1, Length: 1})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusBadRequest {
		t.Fatalf("offset beyond EOF status %v, want bad request", resp.Status)
	}
}

func ReadResponseAfterRequest(conn *common.VsockConn, req Request) (Response, error) {
	if err := WriteRequest(conn, req); err != nil {
		return Response{}, err
	}
	return ReadResponse(conn)
}

func TestServerUnavailable(t *testing.T) {
	conn := startUpdateServer(t, &Server{})
	resp, err := ReadResponseAfterRequest(conn, Request{Command: CommandGetVersion})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusUnavailable || len(resp.Payload) == 0 {
		t.Fatalf("unavailable response: %+v", resp)
	}
}

func TestClientRejectsDigestMismatchAndEmptyChunk(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	pkg := testPackage(t, data, "2026.11.24-1")
	meta := pkg.Metadata()
	meta.SHA256[0]++
	c := testClient(t, &Server{Package: pkg})
	if err := c.Download(context.Background(), meta, &bytes.Buffer{}); err == nil || !bytes.Contains([]byte(err.Error()), []byte("does not match")) {
		t.Fatalf("digest mismatch error: %v", err)
	}

	empty := &Client{}
	empty.dial = func() (*common.VsockConn, error) {
		clientConn, serverConn := updateSocketpair(t)
		go func() {
			defer func() { _ = serverConn.Close() }()
			data, err := serverConn.RecvData()
			if err != nil {
				return
			}
			req, err := ParseRequest(data)
			if err != nil {
				return
			}
			_ = WriteResponse(serverConn, Response{Command: req.Command, Offset: req.Offset, Payload: nil})
			_, _ = serverConn.RecvData()
		}()
		return clientConn, nil
	}
	if err := empty.Download(context.Background(), pkg.Metadata(), &bytes.Buffer{}); err == nil || !bytes.Contains([]byte(err.Error()), []byte("invalid chunk length")) {
		t.Fatalf("empty chunk error: %v", err)
	}
}

func TestClientConcurrentDownloads(t *testing.T) {
	data := bytes.Repeat([]byte("parallel"), MaxChunkSize/8+17)
	pkg := testPackage(t, data, "2026.11.24-1")
	c := testClient(t, &Server{Package: pkg})
	meta := pkg.Metadata()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var dst bytes.Buffer
			if err := c.Download(context.Background(), meta, &dst); err != nil {
				errs <- err
			} else if !bytes.Equal(dst.Bytes(), data) {
				errs <- errors.New("concurrent download differs")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestGetMetadataHonoursContext(t *testing.T) {
	c := &Client{}
	c.dial = func() (*common.VsockConn, error) {
		clientConn, serverConn := updateSocketpair(t)
		go func() {
			defer func() { _ = serverConn.Close() }()
			_, _ = serverConn.RecvData()
			// Read the request but never answer. The client deadline must end it.
			time.Sleep(time.Second)
		}()
		return clientConn, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.GetMetadata(ctx)
	if err == nil {
		t.Fatal("metadata request succeeded without a response")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("metadata error %v is not a deadline", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("metadata cancellation took too long: %v", time.Since(start))
	}
}
