package gpumetricsexporter

import (
	"fmt"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/client"
	"go.uber.org/zap"
)

// sendMetrics serializes m to the wire format and sends it to the host over a
// fresh vsock connection, then sends an empty close-frame and closes the
// connection. On failure it records the error (bumping the send-error counter
// and storing the message); on success it stores m as lastMetrics for delta
// computation on the next tick.
func (e *GpuMetricsExporter) sendMetrics(m *gpumetrics.GpuMetrics) error {
	e.log.Info("sendMetrics", zap.Any("metrics", m))

	c, err := client.NewClientConnection(e.log, e.config.ServerPort)
	if err != nil {
		e.recordSendMetricsError(fmt.Sprintf("client.NewClientConnection error: %v", err))
		e.log.Error("NewClientConnection", zap.Error(err))
		return err
	}
	defer func() {
		// send empty data to close connection on other side
		c.SendData(make([]byte, 0))
		c.Close()
	}()

	data, err := m.ToBytes()
	if err != nil {
		e.recordSendMetricsError(fmt.Sprintf("m.ToBytes error: %v", err))
		e.log.Error("Marshal", zap.Error(err))
		return err
	}
	e.log.Info("sendMetrics", zap.Int("data.len", len(data)))

	err = c.SendData(data)
	if err != nil {
		e.recordSendMetricsError(fmt.Sprintf("c.SendData error: %v", err))
		e.log.Error("SendData", zap.Error(err))
		return err
	}

	e.lastMetricsMu.Lock()
	defer e.lastMetricsMu.Unlock()

	e.lastMetrics = m
	return nil
}

// recordSendMetricsError bumps the error counter and stores the last error
// message under lastMetricsMu. Both fields are read by the query goroutine
// (queryMetrics), so the increment and the string assignment must happen
// together under the same lock that guards lastMetrics.
func (e *GpuMetricsExporter) recordSendMetricsError(msg string) {
	e.lastMetricsMu.Lock()
	defer e.lastMetricsMu.Unlock()
	e.sendMetricsErrorCount++
	e.sendMetricsLastError = msg
}
