package gpumetricsreceiver

import (
	"errors"
	"sync"
	"testing"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

var errSentinel = errors.New("consumer failure")

// recordingConsumer records every metrics payload handed to it.
type recordingConsumer struct {
	mu       sync.Mutex
	received []*gpumetrics.GpuMetrics
	err      error
}

func (c *recordingConsumer) OnGpuMetricsReceived(m *gpumetrics.GpuMetrics) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received = append(c.received, m)
	return c.err
}

func (c *recordingConsumer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.received)
}

// socketpairConns builds a sender/receiver VsockConn pair from an AF_UNIX
// socketpair. RecvData/SendData work over raw fds, but RemoteAddr() resolves to
// a non-SockaddrVM address (CID 0) — exactly the low CID the receiver must
// reject — which lets us exercise the guard without real AF_VSOCK.
func socketpairConns(t *testing.T) (sender, receiver *common.VsockConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("unix.Socketpair: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	})
	return common.NewVsockConn(fds[0]), common.NewVsockConn(fds[1])
}

// TestHandleConnectionRejectsLowCID verifies that a connection whose peer CID is
// below 3 (host/well-known) is rejected: the frame is read, but the metrics
// consumer is never invoked. On a non-vsock socket RemoteAddr().CID is 0.
func TestHandleConnectionRejectsLowCID(t *testing.T) {
	sender, receiverConn := socketpairConns(t)

	consumer := &recordingConsumer{}
	r := &GpuMetricsReceiver{
		log:             zap.NewNop(),
		metricsConsumer: consumer,
		connections:     make(map[*common.VsockConn]struct{}),
	}

	// Send a valid metrics frame followed by the close frame.
	m := gpumetrics.NewGpuMetrics()
	m.Source.InstanceID = "should-not-be-delivered"
	data, err := m.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}
	if err := sender.SendData(data); err != nil {
		t.Fatalf("SendData: %v", err)
	}
	if err := sender.SendData([]byte{}); err != nil { // close frame
		t.Fatalf("SendData(close): %v", err)
	}

	r.wg.Add(1)
	done := make(chan struct{})
	go func() {
		r.handleConnection(receiverConn)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConnection did not return within timeout")
	}

	if consumer.count() != 0 {
		t.Fatalf("consumer was invoked %d time(s) for a low-CID connection; expected 0", consumer.count())
	}
}

// TestProcessFrameHappyPath verifies the decode→stamp→dispatch path: a valid
// frame from a CID >= 3 peer is decoded, stamped with the peer CID, and handed
// to the consumer.
func TestProcessFrameHappyPath(t *testing.T) {
	consumer := &recordingConsumer{}
	r := &GpuMetricsReceiver{
		log:             zap.NewNop(),
		metricsConsumer: consumer,
		connections:     make(map[*common.VsockConn]struct{}),
	}

	m := gpumetrics.NewGpuMetrics()
	m.Source.InstanceID = "i-happy"
	m.ExporterInfo.Seqno = 42
	data, err := m.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}

	const cid uint32 = 7
	if !r.processFrame(cid, data) {
		t.Fatal("processFrame returned false for a valid frame; expected true")
	}
	if consumer.count() != 1 {
		t.Fatalf("consumer invoked %d time(s); expected 1", consumer.count())
	}

	got := consumer.received[0]
	if got.Source.VsockClientID != cid {
		t.Fatalf("VsockClientID = %d, want %d", got.Source.VsockClientID, cid)
	}
	if got.Source.InstanceID != "i-happy" {
		t.Fatalf("InstanceID = %q, want %q", got.Source.InstanceID, "i-happy")
	}
	if got.ExporterInfo.Seqno != 42 {
		t.Fatalf("Seqno = %d, want 42", got.ExporterInfo.Seqno)
	}
}

func TestProcessFrameRejectsLowCID(t *testing.T) {
	consumer := &recordingConsumer{}
	r := &GpuMetricsReceiver{
		log:             zap.NewNop(),
		metricsConsumer: consumer,
		connections:     make(map[*common.VsockConn]struct{}),
	}

	m := gpumetrics.NewGpuMetrics()
	data, err := m.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}

	// CID 2 is the host — must be rejected without invoking the consumer.
	if r.processFrame(2, data) {
		t.Fatal("processFrame returned true for CID 2; expected false")
	}
	if consumer.count() != 0 {
		t.Fatalf("consumer invoked %d time(s); expected 0", consumer.count())
	}
}

func TestProcessFrameEmptyData(t *testing.T) {
	consumer := &recordingConsumer{}
	r := &GpuMetricsReceiver{
		log:             zap.NewNop(),
		metricsConsumer: consumer,
		connections:     make(map[*common.VsockConn]struct{}),
	}

	if r.processFrame(7, nil) {
		t.Fatal("processFrame returned true for nil data; expected false")
	}
	if r.processFrame(7, []byte{}) {
		t.Fatal("processFrame returned true for empty data; expected false")
	}
	if consumer.count() != 0 {
		t.Fatalf("consumer invoked %d time(s); expected 0", consumer.count())
	}
}

func TestProcessFrameCorruptPayload(t *testing.T) {
	consumer := &recordingConsumer{}
	r := &GpuMetricsReceiver{
		log:             zap.NewNop(),
		metricsConsumer: consumer,
		connections:     make(map[*common.VsockConn]struct{}),
	}

	// Not a valid gzip stream -> decode fails, consumer untouched, loop stops.
	if r.processFrame(7, []byte("not gzip")) {
		t.Fatal("processFrame returned true for corrupt payload; expected false")
	}
	if consumer.count() != 0 {
		t.Fatalf("consumer invoked %d time(s); expected 0", consumer.count())
	}
}

// TestProcessFrameConsumerError verifies that a consumer returning an error is
// logged but does NOT stop the connection loop (processFrame still returns true
// so subsequent frames keep flowing).
func TestProcessFrameConsumerError(t *testing.T) {
	consumer := &recordingConsumer{err: errSentinel}
	r := &GpuMetricsReceiver{
		log:             zap.NewNop(),
		metricsConsumer: consumer,
		connections:     make(map[*common.VsockConn]struct{}),
	}

	m := gpumetrics.NewGpuMetrics()
	data, err := m.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}

	if !r.processFrame(7, data) {
		t.Fatal("processFrame returned false despite consumer error; loop should continue")
	}
	if consumer.count() != 1 {
		t.Fatalf("consumer invoked %d time(s); expected 1 (error is non-fatal)", consumer.count())
	}
}
