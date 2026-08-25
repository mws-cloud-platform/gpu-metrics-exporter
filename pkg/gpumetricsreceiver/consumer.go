package gpumetricsreceiver

import (
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

// GpuMetricsConsumer is the interface implemented by downstream consumers of
// decoded metrics. The receiver calls OnGpuMetricsReceived once per decoded
// payload, on the goroutine serving that connection: a slow consumer stalls
// only its own guest's stream, but it must be safe for concurrent calls from
// several connections.
type GpuMetricsConsumer interface {
	OnGpuMetricsReceived(metrics *gpumetrics.GpuMetrics) error
}

// GpuMetricsLogConsumer is the default GpuMetricsConsumer: it simply logs each
// received metrics payload.
type GpuMetricsLogConsumer struct {
	log *zap.Logger
}

func NewGpuMetricsLogConsumer(log *zap.Logger) *GpuMetricsLogConsumer {
	return &GpuMetricsLogConsumer{log: log.With(zap.String("component", "gpu-metrics-consumer"))}
}

func (c *GpuMetricsLogConsumer) OnGpuMetricsReceived(metrics *gpumetrics.GpuMetrics) error {
	c.log.Debug("metrics", zap.Any("metrics", metrics))
	return nil
}
