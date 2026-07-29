package gpumetricsexporter

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

// GpuMetricsExporterConfig configures the exporter: the host vsock port to send
// to, the collection interval in seconds, the build version stamped onto each
// payload, and a logger.
type GpuMetricsExporterConfig struct {
	ServerPort int
	Log        *zap.Logger
	TickPeriod int
	Version    string
}

// GpuMetricsExporter periodically collects GPU metrics via NVML and ships them
// to the host over vsock. Collection and sending run on separate goroutines
// connected by a buffered queue so a slow send cannot stall NVML collection.
type GpuMetricsExporter struct {
	log                   *zap.Logger
	queryMetricsTicker    *time.Ticker
	stopQueryMetricsChan  chan struct{}
	stopSendMetricsChan   chan struct{}
	wg                    sync.WaitGroup
	metricsQueue          chan *gpumetrics.GpuMetrics
	seqno                 atomic.Int64
	config                GpuMetricsExporterConfig
	startTime             int64
	instanceID            string
	initNVMLError         string
	sendMetricsErrorCount int64
	sendMetricsLastError  string
	lastMetricsMu         sync.RWMutex
	lastMetrics           *gpumetrics.GpuMetrics
}

const (
	metricsQueueCapacity = 32
)

// NewGpuMetricsExporter constructs an exporter from config. A non-positive
// TickPeriod defaults to 10 seconds.
func NewGpuMetricsExporter(config GpuMetricsExporterConfig) *GpuMetricsExporter {
	log := config.Log.With(zap.String("component", "gpu-metrics-exporter"))

	// Set default tick period if not specified
	tickPeriod := config.TickPeriod
	if tickPeriod <= 0 {
		tickPeriod = 10 // default to 10 seconds
	}

	queryPeriod := time.Duration(tickPeriod) * time.Second
	e := &GpuMetricsExporter{
		queryMetricsTicker:   time.NewTicker(queryPeriod),
		stopQueryMetricsChan: make(chan struct{}, 1),
		stopSendMetricsChan:  make(chan struct{}, 1),
		log:                  log,
		metricsQueue:         make(chan *gpumetrics.GpuMetrics, metricsQueueCapacity),
		config:               config,
	}
	// Initialize seqno to 0
	e.seqno.Store(0)
	return e
}

// StopOnSignal requests a graceful shutdown of both the query and send loops.
// It is non-blocking and safe to call more than once (e.g. SIGINT then
// SIGTERM): stop channels are buffered and signals are dropped if already
// pending.
func (e *GpuMetricsExporter) StopOnSignal() {
	e.log.Info("stop on signal")

	e.queryMetricsTicker.Stop()
	// Non-blocking: a second signal (e.g. SIGINT then SIGTERM) would otherwise
	// park here forever on a full buffered channel whose reader has already
	// returned.
	signalStop := func(ch chan<- struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	signalStop(e.stopQueryMetricsChan)
	signalStop(e.stopSendMetricsChan)
}

func (e *GpuMetricsExporter) queryMetricsLoop() {
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
			case <-e.stopQueryMetricsChan:
				e.log.Info("received stop signal")
				return
			}
		case <-e.stopQueryMetricsChan:
			e.log.Info("received stop signal")
			return
		}
	}
}

func (e *GpuMetricsExporter) sendMetricsLoop() {
	defer e.wg.Done()

	e.log.Info("starting send metrics loop")

	for {
		select {
		case m := <-e.metricsQueue:
			e.sendMetrics(m)
		case <-e.stopSendMetricsChan:
			e.log.Info("received stop signal")
			return
		}
	}
}

// Run starts the exporter: initializes NVML, launches the send goroutine, and
// runs the query loop in the calling goroutine until StopOnSignal is invoked.
// It blocks until both loops have stopped and NVML has been shut down.
func (e *GpuMetricsExporter) Run() error {
	e.log.Info("run", zap.Int("ServerPort", e.config.ServerPort), zap.Int("TickPeriod", e.config.TickPeriod))
	e.startTime = time.Now().UTC().Unix()

	err := e.initNVML()
	if err != nil {
		e.initNVMLError = fmt.Sprintf("e.initNVML error: %v", err)
		e.log.Error("e.initNVML", zap.Error(err))
	}
	defer func() {
		if e.initNVMLError == "" {
			e.shutdownNVML()
		}
	}()

	e.wg.Add(1)
	go e.sendMetricsLoop()

	e.queryMetricsLoop()

	e.log.Info("wait wg")
	e.wg.Wait()
	e.log.Info("stopped")

	return nil
}
