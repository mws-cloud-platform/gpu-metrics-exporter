package gpumetricsreceiver

import (
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

// GpuMeticsConsumer is the interface implemented by downstream consumers of
// decoded metrics. (The misspelling of "Metrics" is intentional/legacy: it is a
// public API name and renaming it is a breaking change for out-of-tree
// consumers.)
type GpuMeticsConsumer interface {
	OnGpuMetricsReceived(metrics *gpumetrics.GpuMetrics) error
}

// GpuMetricsLogConsumer is the default GpuMeticsConsumer: it simply logs each
// received metrics payload.
type GpuMetricsLogConsumer struct {
	log *zap.Logger
}

func NewGpuMetricsLogConsumer(log *zap.Logger) *GpuMetricsLogConsumer {
	return &GpuMetricsLogConsumer{log: log.With(zap.String("component", "gpu-metrics-consumer"))}
}

func (c *GpuMetricsLogConsumer) OnGpuMetricsReceived(metrics *gpumetrics.GpuMetrics) error {
	c.log.Info("metrics", zap.Any("metrics", metrics))
	return nil
}
