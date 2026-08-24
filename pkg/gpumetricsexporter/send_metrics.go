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
// and storing the message). On success it drains the XID/SXID buffer: the lines
// carried by m are marked delivered and dropped from the unsent queue so they
// are not re-shipped on later ticks. The delta baseline (lastMetrics) is
// maintained by queryMetrics from the last *collected* snapshot, not here.
func (e *GpuMetricsExporter) sendMetrics(m *gpumetrics.GpuMetrics) error {
	e.log.Debug("sendMetrics", zap.Any("metrics", m))

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
	e.log.Debug("sendMetrics", zap.Int("data.len", len(data)))

	err = c.SendData(data)
	if err != nil {
		e.recordSendMetricsError(fmt.Sprintf("c.SendData error: %v", err))
		e.log.Error("SendData", zap.Error(err))
		return err
	}

	// Delivery confirmed: the XID/SXID lines this payload carried are now
	// delivered, so retire them from the unsent buffer and remember them as
	// sent to keep the overlapping dmesg window from re-shipping duplicates.
	// Only the lines actually in m are drained; lines buffered after this
	// payload was collected remain pending for the next tick.
	e.lastMetricsMu.Lock()
	defer e.lastMetricsMu.Unlock()

	for _, line := range m.XIDErrors.XIDErrors {
		if e.sentXIDErrors == nil {
			e.sentXIDErrors = make(map[string]struct{})
		}
		e.sentXIDErrors[line] = struct{}{}
	}
	if len(m.XIDErrors.XIDErrors) > 0 {
		e.unsentXIDErrors = removeStrings(e.unsentXIDErrors, m.XIDErrors.XIDErrors)
	}
	return nil
}

// removeStrings returns dst with every string in drop removed. Order of the
// remaining elements is preserved.
func removeStrings(dst []string, drop []string) []string {
	if len(drop) == 0 {
		return dst
	}
	dropSet := make(map[string]struct{}, len(drop))
	for _, s := range drop {
		dropSet[s] = struct{}{}
	}
	kept := dst[:0]
	for _, s := range dst {
		if _, ok := dropSet[s]; ok {
			continue
		}
		kept = append(kept, s)
	}
	return kept
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
