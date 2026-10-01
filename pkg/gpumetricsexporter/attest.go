package gpumetricsexporter

import (
	"errors"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/attest"
	"go.uber.org/zap"
)

// attestSendTimeout bounds one attested send. The device paces sends with a
// busy bit that, under KVM, is normally clear by the time the doorbell write
// returns; a device that stays busy must not stall the vsock send queued
// behind it, so past this the payload is given up on.
const attestSendTimeout = 3 * time.Second

// attester is the part of *attest.Device the exporter uses, so tests can stand
// in for the device.
type attester interface {
	Send(payload []byte, timeout time.Duration) error
	Info() attest.Info
	Close() error
}

// openAttestDevice is the production openAttestFn.
func openAttestDevice() (attester, error) {
	dev, err := attest.Open()
	if err != nil {
		// A nil *attest.Device must not end up inside a non-nil interface.
		return nil, err
	}
	return dev, nil
}

// openAttest opens the pci-attest device once for the exporter's lifetime.
// Most guests have none -- their QEMU lacks the device -- so its absence is
// logged once at Info; a device that is there but cannot be opened is a
// misconfiguration and logged as an error. Either way the attested channel is
// off for this run and sendAttested becomes a no-op.
func (e *GpuMetricsExporter) openAttest() {
	dev, err := e.openAttestFn()
	switch {
	case errors.Is(err, attest.ErrNoDevice):
		e.log.Info("attested channel disabled", zap.Error(err))
		return
	case err != nil:
		e.log.Error("attest.Open, attested channel disabled", zap.Error(err))
		return
	}
	e.log.Info("attested channel enabled", zap.String("slot", dev.Info().Slot))
	e.attestDev = dev
}

// closeAttest releases the device opened by openAttest. Run calls it only
// after the send loop has exited.
func (e *GpuMetricsExporter) closeAttest() {
	if e.attestDev == nil {
		return
	}
	if err := e.attestDev.Close(); err != nil {
		e.log.Warn("attest Close", zap.Error(err))
	}
	e.attestDev = nil
}

// sendAttested hands the wire bytes to the pci-attest device, if one is open.
// Best effort: the device never reports a verdict, so success only means
// "submitted", and a failure here must not hold up the vsock send.
func (e *GpuMetricsExporter) sendAttested(data []byte) {
	if e.attestDev == nil {
		return
	}
	if err := e.attestDev.Send(data, attestSendTimeout); err != nil {
		e.log.Error("attest Send", zap.Error(err))
	}
}
