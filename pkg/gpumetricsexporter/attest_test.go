package gpumetricsexporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/attest"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeAttester stands in for *attest.Device and records what it is handed.
type fakeAttester struct {
	mu       sync.Mutex
	payloads [][]byte
	timeouts []time.Duration
	closes   int
}

func (f *fakeAttester) Send(payload []byte, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payloads = append(f.payloads, bytes.Clone(payload))
	f.timeouts = append(f.timeouts, timeout)
	return nil
}

func (f *fakeAttester) Info() attest.Info { return attest.Info{Slot: "0000:00:04.0"} }

func (f *fakeAttester) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

// runStopped runs e with an already-cancelled context: Run opens the device,
// starts and stops both loops without a tick, and closes it again.
func runStopped(t *testing.T, e *GpuMetricsExporter) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestSendMetricsAttestsWireBytes pins what reaches the pci-attest device: the
// exact bytes the vsock frame carries -- the host matches one against the
// other -- under attestSendTimeout. There is no vsock host in a test
// environment, so the connection fails; the attested copy must go out anyway,
// since the host reads it over QMP rather than vsock.
func TestSendMetricsAttestsWireBytes(t *testing.T) {
	dev := &fakeAttester{}
	e := &GpuMetricsExporter{log: zap.NewNop(), attestDev: dev}
	m := gpumetrics.NewGpuMetrics()
	m.GpuDeviceCount = 8
	want, err := m.ToBytes()
	if err != nil {
		t.Fatalf("ToBytes: %v", err)
	}

	_ = e.sendMetrics(m)

	if len(dev.payloads) != 1 {
		t.Fatalf("device got %d payloads, want 1", len(dev.payloads))
	}
	if !bytes.Equal(dev.payloads[0], want) {
		t.Errorf("device got %d bytes that differ from the %d-byte wire payload",
			len(dev.payloads[0]), len(want))
	}
	if dev.timeouts[0] != attestSendTimeout {
		t.Errorf("send timeout = %v, want %v", dev.timeouts[0], attestSendTimeout)
	}
}

// TestRunOpensAttestOnceAndClosesIt pins the device's lifetime: opened once
// when Run starts -- not per send, which cost a sysfs scan, a BAR mmap and an
// mlock of the whole image on every tick -- and closed when Run returns.
func TestRunOpensAttestOnceAndClosesIt(t *testing.T) {
	dev := &fakeAttester{}
	opens := 0
	e := NewGpuMetricsExporter(GpuMetricsExporterConfig{Log: zap.NewNop(), TickPeriod: time.Hour})
	e.openAttestFn = func() (attester, error) {
		opens++
		return dev, nil
	}

	runStopped(t, e)

	if opens != 1 {
		t.Errorf("device opened %d times, want 1", opens)
	}
	if dev.closes != 1 {
		t.Errorf("device closed %d times, want 1", dev.closes)
	}
	if e.attestDev != nil {
		t.Error("attestDev still set after Run returned")
	}
}

// TestRunWithoutAttestDevice covers the open failing. A guest without the
// device is the common case and must not log an error -- opening on every
// send used to log one per tick on every such VM. A device that is there but
// cannot be opened is a misconfiguration and is reported, once.
func TestRunWithoutAttestDevice(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantErrors int
	}{
		{"no device", fmt.Errorf("%w (1234:11e9) in this guest", attest.ErrNoDevice), 0},
		{"device cannot be opened", errors.New("mmap BAR: operation not permitted"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			e := NewGpuMetricsExporter(GpuMetricsExporterConfig{Log: zap.New(core), TickPeriod: time.Hour})
			e.openAttestFn = func() (attester, error) { return nil, tc.err }

			runStopped(t, e)

			if got := logs.FilterLevelExact(zap.ErrorLevel).Len(); got != tc.wantErrors {
				t.Errorf("logged %d errors, want %d", got, tc.wantErrors)
			}
			if e.attestDev != nil {
				t.Error("attestDev set although the open failed")
			}
		})
	}
}
