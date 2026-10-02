package gpumetricsexporter

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

// GpuMetricsExporterConfig configures the exporter: the host vsock port to send
// to, the collection interval, the build version stamped onto each payload, and
// a logger.
type GpuMetricsExporterConfig struct {
	ServerPort     int
	Log            *zap.Logger
	TickPeriod     time.Duration
	Version        string
	NvmlLibPath    string
	InstanceIDPath string
	HostRoot       string
}

// GpuMetricsExporter periodically collects GPU metrics via NVML and ships them
// to the host over vsock. Collection and sending run on separate goroutines
// connected by a buffered queue so a slow send cannot stall NVML collection.
type GpuMetricsExporter struct {
	log                *zap.Logger
	queryMetricsTicker *time.Ticker
	wg                 sync.WaitGroup
	metricsQueue       chan *gpumetrics.GpuMetrics
	seqno              atomic.Int64
	config             GpuMetricsExporterConfig
	startTime          int64
	nvmlInitialized    bool
	initNVMLError      string
	nvmlClient         nvml.Interface
	lastNvmlLibPath    string
	nvmlPathLogged     bool
	// onVersionMismatch exits for the supervisor to restart the exporter, once
	// versionMismatch has found that a restart would load another library;
	// tests swap it.
	onVersionMismatch func(ret nvml.Return, staleLibrary string)
	// initNVMLFn/shutdownNVMLFn are the NVML lifecycle calls behind
	// initNVMLForTick/shutdownNVML, swappable in tests to drive the
	// failure/retry/recovery paths without a GPU.
	initNVMLFn            func() error
	shutdownNVMLFn        func()
	sendMetricsErrorCount int64
	sendMetricsLastError  string
	lastMetricsMu         sync.RWMutex
	lastMetrics           *gpumetrics.GpuMetrics
	// XID/SXID error-line tracking. unsentXIDErrors holds lines read from the
	// kernel log whose carrying payload has not yet been confirmed delivered:
	// they ride along on every subsequent payload until a send succeeds, so a
	// transient send failure cannot silently drop an XID. xidErrorsDropped
	// counts lines evicted on overflow and is reported on the wire as
	// XIDErrors.DroppedCount. Both are guarded by lastMetricsMu.
	unsentXIDErrors  []string
	xidErrorsDropped int64
	// kmsg reads the kernel log the XID lines come from. It belongs to the
	// query goroutine; Run closes it once the query loop has exited.
	kmsg *kmsgReader
	// attestDev is the pci-attest device, opened once by Run before the send
	// loop starts and closed after it exits, so only the send goroutine uses
	// it in between. nil when the guest has none or it could not be opened.
	// openAttestFn is the open behind it, swappable in tests.
	attestDev    attester
	openAttestFn func() (attester, error)
	// nvmlLib measures the libnvidia-ml mapped into the process for each
	// payload's nvml_library; lastNVMLLibraryReport is what was last logged,
	// so a finding is logged when it changes rather than every tick. Both
	// belong to the query goroutine.
	nvmlLib               *nvmlLibraryInspector
	lastNVMLLibraryReport string
	// nvidiaDriverVersionPath is where the NVIDIA kernel module reports its
	// version, swappable in tests.
	nvidiaDriverVersionPath string
}

const (
	metricsQueueCapacity = 32

	// maxUnsentXIDErrors bounds the XID/SXID re-ship buffer. The buffer must be
	// bounded: while the host receiver is unreachable, every XID line observed
	// accumulates and the whole set rides on every payload, growing it until
	// the gzipped frame passes the 64 KiB vsock limit — at which point SendData
	// fails on size for good and the exporter never recovers, even once the
	// host returns. On overflow the oldest lines go first (the newest describe
	// the current fault) and the loss is counted, never silent.
	maxUnsentXIDErrors = 128

	// tracerPollInterval is how often watchTracer samples TracerPid. Far
	// shorter than a tick (60 s deployed) so a debugger that attaches and
	// detaches between ticks is still counted, yet just one small procfs read
	// each time, so the cost is negligible. It bounds, but cannot close, the
	// window in which a very brief attach slips by unseen.
	tracerPollInterval = 250 * time.Millisecond

	// xidStartupLookbackSlack is added to TickPeriod to bound how far back the
	// first read of the kernel log reaches: the window the exporter used to
	// give dmesg on every tick, so a restart neither re-ships the whole ring
	// buffer nor skips what was logged just before it.
	xidStartupLookbackSlack = 10 * time.Second
)

// NewGpuMetricsExporter constructs an exporter from config. A non-positive
// TickPeriod defaults to 10 seconds.
func NewGpuMetricsExporter(config GpuMetricsExporterConfig) *GpuMetricsExporter {
	log := config.Log.With(zap.String("component", "gpu-metrics-exporter"))

	// Set default tick period if not specified
	tickPeriod := config.TickPeriod
	if tickPeriod <= 0 {
		tickPeriod = 10 * time.Second // default to 10 seconds
	}

	if config.InstanceIDPath == "" {
		config.InstanceIDPath = cloudInitInstanceIDFilePath
	}

	e := &GpuMetricsExporter{
		queryMetricsTicker:      time.NewTicker(tickPeriod),
		log:                     log,
		metricsQueue:            make(chan *gpumetrics.GpuMetrics, metricsQueueCapacity),
		config:                  config,
		nvidiaDriverVersionPath: nvidiaDriverVersionFile,
	}
	// NVML lifecycle defaults; tests swap these to drive the
	// failure/retry/recovery paths without a GPU.
	e.initNVMLFn = e.initNVML
	e.shutdownNVMLFn = e.shutdownNVMLReal
	e.openAttestFn = openAttestDevice
	e.nvmlLib = newNVMLLibraryInspector(config)
	e.kmsg = newKmsgReader(log)
	e.onVersionMismatch = func(ret nvml.Return, staleLibrary string) {
		e.log.Fatal("NVML driver/library version mismatch detected, exiting for supervisor restart",
			zap.String("error", nvml.ErrorString(ret)), zap.String("stale_library", staleLibrary))
	}
	// Initialize seqno to 0
	e.seqno.Store(0)
	return e
}

func (e *GpuMetricsExporter) queryMetricsLoop(ctx context.Context) {
	e.log.Info("starting query metrics loop")

	for {
		select {
		case <-e.queryMetricsTicker.C:
			m, err := e.queryMetrics()
			if err != nil {
				e.log.Error("queryMetrics", zap.Error(err))
				continue
			}
			// Select on the stop channel while enqueuing: if the queue (cap 32)
			// is full because the send loop is stalled, a stop signal can still
			// interrupt us here instead of parking forever on metricsQueue <- m.
			select {
			case e.metricsQueue <- m:
			case <-ctx.Done():
				e.log.Info("received stop signal")
				return
			}
		case <-ctx.Done():
			e.log.Info("received stop signal")
			return
		}
	}
}

func (e *GpuMetricsExporter) sendMetricsLoop(ctx context.Context) {
	defer e.wg.Done()

	e.log.Info("starting send metrics loop")

	for {
		select {
		case m := <-e.metricsQueue:
			_ = e.sendMetrics(m)
		case <-ctx.Done():
			e.log.Info("received stop signal")
			return
		}
	}
}

// watchTracer polls for a debugger attached to the exporter far more often
// than the tick, so inspect's cumulative count catches an attach-and-detach
// that would fall between two ticks. It samples once right away -- an attach
// present at startup counts -- then on every tracerPollInterval until the
// context is done.
func (e *GpuMetricsExporter) watchTracer(ctx context.Context) {
	defer e.wg.Done()
	if e.nvmlLib == nil {
		return
	}
	e.nvmlLib.pollTracer()
	t := time.NewTicker(tracerPollInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			e.nvmlLib.pollTracer()
		case <-ctx.Done():
			return
		}
	}
}

// Run starts the exporter: launches the send goroutine and runs the query
// loop in the calling goroutine until the context is done. NVML is
// initialized and shut down around every collection tick (see
// initNVMLForTick), so a failed init is retried on the next tick and open
// handles to device files are released between ticks. On a driver/library
// version mismatch the exporter exits with a non-zero code for its supervisor
// (systemd or kubelet) to restart it with a fresh address space, if that would
// load another library, and otherwise reports the mismatch and carries on (see
// versionMismatch). The pci-attest device, by contrast, is opened once here
// for the whole run.
func (e *GpuMetricsExporter) Run(ctx context.Context) error {
	defer e.queryMetricsTicker.Stop()

	e.log.Info("run", zap.Int("ServerPort", e.config.ServerPort), zap.Duration("TickPeriod", e.config.TickPeriod))
	e.startTime = time.Now().UTC().Unix()

	e.openAttest()

	e.wg.Add(1)
	go e.sendMetricsLoop(ctx)

	e.wg.Add(1)
	go e.watchTracer(ctx)

	e.queryMetricsLoop(ctx)
	e.kmsg.closeDevice()

	e.log.Info("wait wg")
	e.wg.Wait()
	e.closeAttest()
	e.log.Info("stopped")

	return nil
}
