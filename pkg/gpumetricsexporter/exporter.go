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
	onVersionMismatch  func(nvml.Return)
	// initNVMLFn/shutdownNVMLFn are the NVML lifecycle calls behind
	// initNVMLForTick/shutdownNVML, swappable in tests to drive the
	// failure/retry/recovery paths without a GPU.
	initNVMLFn            func() error
	shutdownNVMLFn        func()
	sendMetricsErrorCount int64
	sendMetricsLastError  string
	lastMetricsMu         sync.RWMutex
	lastMetrics           *gpumetrics.GpuMetrics
	// XID/SXID error-line tracking. unsentXIDErrors holds lines that have been
	// observed in dmesg but whose carrying payload has not yet been confirmed
	// delivered: they ride along on every subsequent payload until a send
	// succeeds, so a transient send failure (or a send stall lasting longer
	// than the dmesg look-back window) cannot silently drop an XID.
	// retiredXIDErrors maps a line that has left the pending buffer — whether
	// delivered or dropped on overflow — to when it left, so the overlapping
	// dmesg window cannot re-admit it; records are pruned once they age out of
	// that window. Dropped lines belong in here too: a line that is neither
	// pending nor retired looks fresh on the next tick and gets re-queued
	// behind newer lines, which then get evicted in its place.
	// xidErrorsDropped counts lines evicted on overflow and is reported on the
	// wire as XIDErrors.DroppedCount. All three are guarded by lastMetricsMu.
	unsentXIDErrors  []string
	retiredXIDErrors map[string]time.Time
	xidErrorsDropped int64
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

	// maxRetiredXIDErrors backstops the retired-line set for the case where a
	// single dmesg window carries more distinct lines than age-based pruning
	// retires.
	maxRetiredXIDErrors = 1024

	// xidDmesgWindowSlack is added to TickPeriod to form the dmesg look-back
	// window, so consecutive ticks overlap and no line falls between them.
	xidDmesgWindowSlack = 10 * time.Second
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
		queryMetricsTicker: time.NewTicker(tickPeriod),
		log:                log,
		metricsQueue:       make(chan *gpumetrics.GpuMetrics, metricsQueueCapacity),
		config:             config,
	}
	// NVML lifecycle defaults; tests swap these to drive the
	// failure/retry/recovery paths without a GPU.
	e.initNVMLFn = e.initNVML
	e.shutdownNVMLFn = e.shutdownNVMLReal
	e.openAttestFn = openAttestDevice
	e.nvmlLib = newNVMLLibraryInspector(config)
	e.onVersionMismatch = func(ret nvml.Return) {
		e.log.Fatal("NVML driver/library version mismatch detected, exiting for supervisor restart",
			zap.String("error", nvml.ErrorString(ret)))
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

// Run starts the exporter: launches the send goroutine and runs the query
// loop in the calling goroutine until the context is done. NVML is
// initialized and shut down around every collection tick (see
// initNVMLForTick), so a failed init is retried on the next tick and open
// handles to device files are released between ticks. If a driver upgrade
// causes a driver/library version mismatch, the exporter exits with a non-zero
// code so its supervisor (systemd or kubelet) can restart it with a fresh address space.
// The pci-attest device, by contrast, is opened once here for the whole run.
func (e *GpuMetricsExporter) Run(ctx context.Context) error {
	defer e.queryMetricsTicker.Stop()

	e.log.Info("run", zap.Int("ServerPort", e.config.ServerPort), zap.Duration("TickPeriod", e.config.TickPeriod))
	e.startTime = time.Now().UTC().Unix()

	e.openAttest()

	e.wg.Add(1)
	go e.sendMetricsLoop(ctx)

	e.queryMetricsLoop(ctx)

	e.log.Info("wait wg")
	e.wg.Wait()
	e.closeAttest()
	e.log.Info("stopped")

	return nil
}
