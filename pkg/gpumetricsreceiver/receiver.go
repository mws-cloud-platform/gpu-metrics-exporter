package gpumetricsreceiver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/exporterupdate"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/server"
	"go.uber.org/zap"
)

// GpuMetricsReceiverConfig configures the receiver: the vsock port to listen
// on, a logger, the consumer that receives each decoded metrics payload, and
// resource limits bounding untrusted guest connections.
//
// MaxConnections bounds the total number of simultaneously-served guest
// connections. MaxConnectionsPerCID bounds concurrent connections per guest VM
// (by peer CID). ReadTimeout bounds how long a single framed read may block
// before the connection is dropped, so a guest that opens a connection and then
// stalls cannot pin a goroutine and fd. WriteTimeout does the same for update
// responses to a guest that stops reading.
//
// For all four, a zero value means "use the default" — NewGpuMetricsReceiver
// substitutes one, so an under-specified config is still bounded against
// untrusted guests. Pass a negative value to disable a limit outright; that is
// the only way to opt out, and it should be a deliberate choice.
type GpuMetricsReceiverConfig struct {
	ListenPort           int
	Log                  *zap.Logger
	MetricsConsumer      GpuMetricsConsumer
	MaxConnections       int
	MaxConnectionsPerCID int
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	// ExporterDebPath and ExporterVersion enable host-side package updates.
	// They must be set together; both empty serves metrics only. The version is
	// supplied by the operator because the receiver never parses the .deb or
	// executes dpkg-deb (it may run in a minimal pod).
	ExporterDebPath string
	ExporterVersion string
}

// GpuMetricsReceiver accepts exporter connections over vsock, decodes each
// framed GpuMetrics payload, and hands it to the configured consumer. Shutdown
// is coordinated through context cancellation plus Close.
type GpuMetricsReceiver struct {
	config               GpuMetricsReceiverConfig
	log                  *zap.Logger
	metricsConsumer      GpuMetricsConsumer
	stopping             atomic.Int32
	wg                   sync.WaitGroup
	listener             *server.VsockListener
	connections          map[*common.VsockConn]struct{}
	cidCounts            map[uint32]int
	connectionsMu        sync.Mutex
	maxConnections       int
	maxConnectionsPerCID int
	readTimeout          time.Duration
	writeTimeout         time.Duration
	updateServer         *exporterupdate.Server
}

// NewGpuMetricsReceiver constructs a receiver from config. Zero-valued
// resource limits are replaced with defaults so a misconfigured receiver still
// bounds untrusted guest connections; negative values are left alone and read
// as "no limit" at the enforcement sites, which all test for > 0.
func NewGpuMetricsReceiver(config GpuMetricsReceiverConfig) *GpuMetricsReceiver {
	log := config.Log.With(zap.String("component", "gpu-metrics-receiver"))
	if config.MaxConnections == 0 {
		config.MaxConnections = defaultMaxConnections
	}
	if config.MaxConnectionsPerCID == 0 {
		config.MaxConnectionsPerCID = defaultMaxConnectionsPerCID
	}
	if config.ReadTimeout == 0 {
		config.ReadTimeout = defaultReadTimeout
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	r := &GpuMetricsReceiver{
		log:                  log,
		config:               config,
		metricsConsumer:      config.MetricsConsumer,
		connections:          make(map[*common.VsockConn]struct{}),
		cidCounts:            make(map[uint32]int),
		maxConnections:       config.MaxConnections,
		maxConnectionsPerCID: config.MaxConnectionsPerCID,
		readTimeout:          config.ReadTimeout,
		writeTimeout:         config.WriteTimeout,
	}
	r.stopping.Store(0)
	return r
}

// Resource-limit defaults applied by NewGpuMetricsReceiver when the config
// leaves them at zero. They are conservative: one VM should not need dozens of
// simultaneous exporter connections, and a frame must arrive well within a
// minute. WriteTimeout likewise stops a guest that no longer reads an update
// response from pinning a serving goroutine.
const (
	defaultMaxConnections       = 64
	defaultMaxConnectionsPerCID = 4
	defaultReadTimeout          = 60 * time.Second
	defaultWriteTimeout         = 60 * time.Second
)

func (r *GpuMetricsReceiver) shutdown() {
	if !r.stopping.CompareAndSwap(0, 1) {
		return
	}

	r.connectionsMu.Lock()
	r.log.Info("closing listen fd")
	if r.listener != nil {
		_ = r.listener.Close()
		r.listener = nil
	}

	r.log.Info("closing connections")
	connections := make([]*common.VsockConn, 0, len(r.connections))
	for conn := range r.connections {
		connections = append(connections, conn)
		delete(r.connections, conn)
	}
	// Clear the per-CID counts alongside the connection set. handleConnection's
	// teardown only decrements when it still finds the conn in r.connections,
	// so draining that map above means those decrements never happen. Run()
	// resets `stopping` and rebuilds the listener, i.e. the receiver is meant
	// to be re-runnable; leaving stale counts here would have every CID that
	// was live at shutdown start the next Run already partway to its per-CID
	// cap — or over it, and rejected outright.
	clear(r.cidCounts)
	r.connectionsMu.Unlock()

	for _, conn := range connections {
		_ = conn.Close()
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

// handleConnection serves one exporter connection. The first frame selects the
// protocol for the connection's lifetime: update magic enters the bounded
// request/response update server, while every other frame follows the existing
// gzip metrics path. Both paths use the same peer-CID and connection accounting.
func (r *GpuMetricsReceiver) handleConnection(conn *common.VsockConn) {
	defer r.wg.Done()

	// Resolve the peer CID once, before the teardown defer closes over it: it's
	// the VM identity check (CID >= 3), the value stamped onto
	// metrics.Source.VsockClientID for downstream CID→VMID mapping, and the key
	// the connection is counted under. Reading it once keeps that identity
	// stable instead of re-querying the kernel per frame and again at teardown,
	// where getpeername can fail and yield a different (zero) answer.
	cid := conn.RemoteAddr().CID

	defer func() {
		r.connectionsMu.Lock()
		_, ok := r.connections[conn]
		if ok {
			delete(r.connections, conn)
			// Release the per-CID slot under the same key serveConnection
			// counted it under. That increment is unconditional, so this
			// decrement must be too: gating it on a re-read of the peer CID
			// would skip the release exactly when the read fails, and the VM
			// would lose a slot permanently. shutdown() clears the whole map
			// for the case where it drained r.connections first.
			if r.cidCounts != nil && r.cidCounts[cid] > 0 {
				r.cidCounts[cid]--
				if r.cidCounts[cid] == 0 {
					delete(r.cidCounts, cid)
				}
			}
		}
		r.connectionsMu.Unlock()

		if ok {
			_ = conn.Close()
		}
	}()
	r.serveFrames(conn, cid)
}

// serveFrames is separated from peer-address lookup and teardown so tests can
// exercise both protocol paths with a real CID over an AF_UNIX socketpair.
func (r *GpuMetricsReceiver) serveFrames(conn *common.VsockConn, cid uint32) {
	r.log.Info("exporter connected", zap.Any("remote", conn.RemoteAddr()))

	firstFrame := true
	for r.stopping.Load() == 0 {
		// Reset the per-frame read deadline so a guest that stalls mid-stream
		// (or after sending just a header) is dropped instead of pinning this
		// goroutine and its fd until process shutdown.
		if r.readTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(r.readTimeout))
		}

		data, err := conn.RecvData()
		if err != nil {
			// other side want stop
			if err == common.ErrNoData {
				break
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				r.log.Warn("read timeout, closing connection", zap.Any("remote", conn.RemoteAddr()))
				break
			}

			r.log.Error("RecvData", zap.Error(err))
			break
		}

		if firstFrame && exporterupdate.IsRequestFrame(data) {
			if cid < 3 {
				r.log.Error("unexpected update clientID", zap.Uint32("clientID", cid))
				break
			}
			server := r.updateServer
			if server == nil {
				server = &exporterupdate.Server{}
			}
			if err := server.Serve(conn, data, r.readTimeout, r.writeTimeout); err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					r.log.Warn("update connection timeout", zap.Uint32("clientID", cid), zap.Error(err))
				} else {
					r.log.Warn("update connection", zap.Uint32("clientID", cid), zap.Error(err))
				}
			}
			break
		}
		firstFrame = false

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
	r.log.Debug("received metrics", zap.Any("metrics", metrics))
	metrics.Source.VsockClientID = cid
	if err := r.metricsConsumer.OnGpuMetricsReceived(metrics); err != nil {
		r.log.Error("metrics consumer", zap.Error(err))
	}
	return true
}

func (r *GpuMetricsReceiver) serveConnection(conn *common.VsockConn) {
	cid := conn.RemoteAddr().CID

	r.connectionsMu.Lock()
	if r.stopping.Load() != 0 {
		r.connectionsMu.Unlock()
		r.log.Info("already stopping -> close accepted connection")
		_ = conn.Close()
		return
	}
	// Bound untrusted guest connections: a misbehaving VM must not be able to
	// exhaust host fds/goroutines by opening many connections (overall cap) or
	// flooding from a single CID (per-CID cap). 0 means no cap.
	if r.maxConnections > 0 && len(r.connections) >= r.maxConnections {
		r.connectionsMu.Unlock()
		r.log.Warn("max connections reached, rejecting connection",
			zap.Int("current", len(r.connections)), zap.Int("max", r.maxConnections),
			zap.Uint32("clientID", cid))
		_ = conn.Close()
		return
	}
	if r.maxConnectionsPerCID > 0 && r.cidCounts != nil && r.cidCounts[cid] >= r.maxConnectionsPerCID {
		r.connectionsMu.Unlock()
		r.log.Warn("max per-CID connections reached, rejecting connection",
			zap.Uint32("clientID", cid), zap.Int("current", r.cidCounts[cid]),
			zap.Int("max", r.maxConnectionsPerCID))
		_ = conn.Close()
		return
	}
	r.connections[conn] = struct{}{}
	if r.cidCounts != nil {
		r.cidCounts[cid]++
	}
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

	var updatePackage *exporterupdate.Package
	switch {
	case r.config.ExporterDebPath == "" && r.config.ExporterVersion == "":
		// Metrics-only receiver; update requests receive StatusUnavailable.
	case r.config.ExporterDebPath == "" || r.config.ExporterVersion == "":
		return fmt.Errorf("exporter update requires both package path and version")
	default:
		pkg, err := exporterupdate.OpenPackage(r.config.ExporterDebPath, r.config.ExporterVersion)
		if err != nil {
			return err
		}
		updatePackage = pkg
		meta := pkg.Metadata()
		r.log.Info("serving exporter update package",
			zap.String("version", meta.Version), zap.Int64("size", meta.Size))
		// Registered before the shutdown defer below, so it runs after all
		// connection goroutines have stopped using the open snapshot.
		defer func() { _ = pkg.Close() }()
	}
	r.updateServer = &exporterupdate.Server{Package: updatePackage}

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
