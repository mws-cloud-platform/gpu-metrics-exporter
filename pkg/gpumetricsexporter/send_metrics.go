package gpumetricsexporter

import (
	"fmt"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/client"
	"go.uber.org/zap"
)

// sendMetrics serializes m to the wire format and sends it to the host over a
// fresh vsock connection, then sends an empty close-frame and closes the
// connection. Before connecting it submits the same bytes to the pci-attest
// device, if one is open (sendAttested): the host reads that copy over QMP,
// not vsock, so it goes out even when the vsock connection fails, and its
// wait is bounded by attestSendTimeout. On a vsock failure it records the
// error (bumping the send-error counter and storing the message). On success
// it clears the last send error — a confirmed delivery is proof the channel
// works, and a stale message from before a receiver restart would otherwise
// ride on every payload forever, reading as an ongoing problem (the counter
// stays cumulative; only the message is transient). It also drains the
// XID/SXID buffer: the lines carried by m are marked delivered and dropped
// from the unsent queue so they are not re-shipped on later ticks. The delta
// baseline (lastMetrics) is maintained by queryMetrics from the last
// *collected* snapshot, not here.
func (e *GpuMetricsExporter) sendMetrics(m *gpumetrics.GpuMetrics) error {
	e.log.Debug("sendMetrics", zap.Any("metrics", m))

	data, err := m.ToBytes()
	if err != nil {
		e.recordSendMetricsError(fmt.Sprintf("m.ToBytes error: %v", err))
		e.log.Error("Marshal", zap.Error(err))
		return err
	}
	e.log.Debug("sendMetrics", zap.Int("data.len", len(data)))

	e.sendAttested(data)

	c, err := client.NewClientConnection(e.log, e.config.ServerPort)
	if err != nil {
		e.recordSendMetricsError(fmt.Sprintf("client.NewClientConnection error: %v", err))
		e.log.Error("NewClientConnection", zap.Error(err))
		return err
	}
	defer func() {
		// send empty data to close connection on other side
		_ = c.SendData(make([]byte, 0))
		_ = c.Close()
	}()

	err = c.SendData(data)
	if err != nil {
		e.recordSendMetricsError(fmt.Sprintf("c.SendData error: %v", err))
		e.log.Error("SendData", zap.Error(err))
		return err
	}

	// Delivery confirmed: clear the last send error so the field means
	// "the channel is broken now" rather than "it was broken at some point"
	// (recordSendMetricsError sets it back on the next failure), and retire
	// the XID/SXID lines this payload carried from the unsent buffer and
	// remember them as sent to keep the overlapping dmesg window from
	// re-shipping duplicates. Only the lines actually in m are drained;
	// lines buffered after this payload was collected remain pending for
	// the next tick.
	e.lastMetricsMu.Lock()
	defer e.lastMetricsMu.Unlock()

	e.clearSendMetricsLastError()

	if len(m.XIDErrors.XIDErrors) > 0 {
		if e.retiredXIDErrors == nil {
			e.retiredXIDErrors = make(map[string]time.Time, len(m.XIDErrors.XIDErrors))
		}
		// Stamp the delivery time so pruneRetiredXIDErrors can drop the record
		// once the line can no longer reappear in the dmesg look-back window.
		now := time.Now()
		for _, line := range m.XIDErrors.XIDErrors {
			e.retiredXIDErrors[line] = now
		}
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

// clearSendMetricsLastError empties the last send error after a confirmed
// delivery, so the field reports "the channel is broken now" rather than
// "it was broken at some point". The counter is deliberately untouched: it
// stays cumulative, so a consumer summing it still sees every failure that
// ever happened.
//
// Callers must hold lastMetricsMu.
func (e *GpuMetricsExporter) clearSendMetricsLastError() {
	e.sendMetricsLastError = ""
}
