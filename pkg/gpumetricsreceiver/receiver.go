package gpumetricsreceiver

import (
	"context"
	"sync"
	"sync/atomic"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/server"
	"go.uber.org/zap"
)

// GpuMetricsReceiverConfig configures the receiver: the vsock port to listen
// on, a logger, and the consumer that receives each decoded metrics payload.
type GpuMetricsReceiverConfig struct {
	ListenPort      int
	Log             *zap.Logger
	MetricsConsumer GpuMeticsConsumer
}

// GpuMetricsReceiver accepts exporter connections over vsock, decodes each
// framed GpuMetrics payload, and hands it to the configured consumer. Shutdown
// is coordinated through context cancellation plus Close.
type GpuMetricsReceiver struct {
	config          GpuMetricsReceiverConfig
	log             *zap.Logger
	metricsConsumer GpuMeticsConsumer
	stopping        atomic.Int32
	wg              sync.WaitGroup
	listener        *server.VsockListener
	connections     map[*common.VsockConn]struct{}
	connectionsMu   sync.Mutex
}

// NewGpuMetricsReceiver constructs a receiver from config.
func NewGpuMetricsReceiver(config GpuMetricsReceiverConfig) *GpuMetricsReceiver {
	log := config.Log.With(zap.String("component", "gpu-metrics-receiver"))
	r := &GpuMetricsReceiver{log: log, config: config, metricsConsumer: config.MetricsConsumer, connections: make(map[*common.VsockConn]struct{})}
	r.stopping.Store(0)
	return r
}

func (r *GpuMetricsReceiver) shutdown() {
	if !r.stopping.CompareAndSwap(0, 1) {
		return
	}

	r.connectionsMu.Lock()
	r.log.Info("closing listen fd")
	if r.listener != nil {
		r.listener.Close()
		r.listener = nil
	}

	r.log.Info("closing connections")
	connections := make([]*common.VsockConn, 0, len(r.connections))
	for conn := range r.connections {
		connections = append(connections, conn)
		delete(r.connections, conn)
	}
	r.connectionsMu.Unlock()

	for _, conn := range connections {
		conn.Close()
	}

	r.log.Info("closed fd")
}

func (r *GpuMetricsReceiver) waitStopped() {
	r.log.Info("waiting wg")
	r.wg.Wait()
	r.log.Info("stopped")
}

func (r *GpuMetricsReceiver) shutdownOnContextDone(ctx context.Context) func() {
	stop := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		select {
		case <-ctx.Done():
			r.log.Info("context done, stopping receiver")
			r.shutdown()
		case <-stop:
		}
	}()

	return func() {
		close(stop)
		<-stopped
	}
}

func (r *GpuMetricsReceiver) setListener(l *server.VsockListener) {
	r.connectionsMu.Lock()
	r.listener = l
	r.connectionsMu.Unlock()
}

// handleConnection serves one exporter connection: reads framed GpuMetrics
// payloads in a loop, dispatching each via processFrame, until the exporter
// sends its close-frame (ErrNoData), the connection breaks, or the receiver is
// stopping.
func (r *GpuMetricsReceiver) handleConnection(conn *common.VsockConn) {
	defer r.wg.Done()
	defer func() {
		r.connectionsMu.Lock()
		_, ok := r.connections[conn]
		if ok {
			delete(r.connections, conn)
		}
		r.connectionsMu.Unlock()

		if ok {
			conn.Close()
		}
	}()

	// Resolve the peer CID once: it's both the VM identity check (CID >= 3) and
	// the value stamped onto metrics.Source.VsockClientID for downstream CID→VMID
	// mapping. Reading it once per connection also keeps a stable identity across
	// the loop instead of re-querying the kernel each frame.
	cid := conn.RemoteAddr().CID
	r.log.Info("exporter connected", zap.Any("remote", conn.RemoteAddr()))

	for r.stopping.Load() == 0 {
		data, err := conn.RecvData()
		if err != nil {
			// other side want stop
			if err == common.ErrNoData {
				break
			}

			r.log.Error("RecvData", zap.Error(err))
			break
		}

		if !r.processFrame(cid, data) {
			break
		}
	}

	r.log.Info("served connection")
}

// processFrame decodes one received frame and dispatches it to the consumer.
// Returns false when the connection loop should terminate (bad frame, rejected
// peer, or a nil payload). Extracted from handleConnection so the decode→stamp→
// dispatch path is unit-testable without a real AF_VSOCK socket.
func (r *GpuMetricsReceiver) processFrame(cid uint32, data []byte) bool {
	if len(data) == 0 {
		r.log.Error("received nothing")
		return false
	}

	if cid < 3 {
		r.log.Error("unexpected clientID", zap.Uint32("clientID", cid))
		return false
	}

	metrics, err := gpumetrics.NewGpuMetricsFromBytes(data)
	if err != nil {
		r.log.Error("gpumetrics.NewGpuMetricsFromBytes", zap.Error(err))
		return false
	}
	r.log.Info("received metrics", zap.Any("metrics", metrics))
	metrics.Source.VsockClientID = cid
	if err := r.metricsConsumer.OnGpuMetricsReceived(metrics); err != nil {
		r.log.Error("metrics consumer", zap.Error(err))
	}
	return true
}

func (r *GpuMetricsReceiver) serveConnection(conn *common.VsockConn) {
	r.connectionsMu.Lock()
	if r.stopping.Load() != 0 {
		r.connectionsMu.Unlock()
		r.log.Info("already stopping -> close accepted connection")
		conn.Close()
		return
	}
	r.connections[conn] = struct{}{}
	r.connectionsMu.Unlock()

	r.wg.Add(1)
	go r.handleConnection(conn)
}

func (r *GpuMetricsReceiver) acceptLoop(ctx context.Context, l *server.VsockListener) {
	for r.stopping.Load() == 0 {
		r.log.Info("accept pending")
		conn, err := l.Accept(ctx)
		r.log.Info("accept done")
		if err != nil {
			if r.stopping.Load() != 0 || ctx.Err() != nil {
				r.log.Info("accept stopped", zap.Error(err))
				return
			}
			r.log.Warn("accept failure", zap.Error(err))
			continue
		}

		r.serveConnection(conn)
	}
}

// Run starts the receiver: opens a vsock listener and runs the accept loop
// until ctx is cancelled or the receiver is stopped. It blocks until all
// in-flight connections have been served and the listener is closed.
func (r *GpuMetricsReceiver) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	l, err := server.NewVsockListener(r.log, r.config.ListenPort)
	if err != nil {
		r.log.Error("NewVsockListener", zap.Error(err))
		return err
	}

	r.stopping.Store(0)
	r.setListener(l)

	stopContextShutdown := r.shutdownOnContextDone(ctx)
	defer func() {
		stopContextShutdown()
		r.shutdown()
		r.waitStopped()
	}()

	r.acceptLoop(ctx, l)
	return nil
}
