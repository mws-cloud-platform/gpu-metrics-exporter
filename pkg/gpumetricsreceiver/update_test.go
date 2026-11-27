package gpumetricsreceiver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mws.cloud/gpu-metrics-exporter/pkg/exporterupdate"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.uber.org/zap"
)

func receiverTestPackage(t *testing.T) *exporterupdate.Package {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exporter.deb")
	if err := os.WriteFile(path, bytes.Repeat([]byte("deb"), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := exporterupdate.OpenPackage(path, "2026.11.24-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pkg.Close() })
	return pkg
}

func TestServeFramesUpdateRequest(t *testing.T) {
	sender, receiverConn := socketpairConns(t)
	r := &GpuMetricsReceiver{
		log:          zap.NewNop(),
		readTimeout:  2 * time.Second,
		writeTimeout: 2 * time.Second,
		updateServer: &exporterupdate.Server{Package: receiverTestPackage(t)},
	}
	done := make(chan struct{})
	go func() {
		r.serveFrames(receiverConn, 7)
		close(done)
	}()
	if err := exporterupdate.WriteRequest(sender, exporterupdate.Request{Command: exporterupdate.CommandGetVersion}); err != nil {
		t.Fatal(err)
	}
	resp, err := exporterupdate.ReadResponse(sender)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != exporterupdate.StatusOK || !bytes.Contains(resp.Payload, []byte("2026.11.24-1")) {
		t.Fatalf("metadata response status=%v payload=%q", resp.Status, resp.Payload)
	}
	_ = sender.SendData(nil)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("update dispatch did not finish")
	}
}

func TestServeFramesRejectsLowCIDUpdate(t *testing.T) {
	sender, receiverConn := socketpairConns(t)
	r := &GpuMetricsReceiver{log: zap.NewNop(), updateServer: &exporterupdate.Server{Package: receiverTestPackage(t)}}
	done := make(chan struct{})
	go func() {
		r.serveFrames(receiverConn, 2)
		close(done)
	}()
	if err := exporterupdate.WriteRequest(sender, exporterupdate.Request{Command: exporterupdate.CommandGetVersion}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("low-CID update request was not rejected")
	}
}

func TestServeFramesUpdateUnavailable(t *testing.T) {
	sender, receiverConn := socketpairConns(t)
	r := &GpuMetricsReceiver{log: zap.NewNop(), readTimeout: 2 * time.Second, writeTimeout: 2 * time.Second}
	done := make(chan struct{})
	go func() {
		r.serveFrames(receiverConn, 7)
		close(done)
	}()
	if err := exporterupdate.WriteRequest(sender, exporterupdate.Request{Command: exporterupdate.CommandGetVersion}); err != nil {
		t.Fatal(err)
	}
	resp, err := exporterupdate.ReadResponse(sender)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != exporterupdate.StatusUnavailable {
		t.Fatalf("status %v, want unavailable", resp.Status)
	}
	_ = sender.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unavailable update dispatch did not finish")
	}
}

func TestServeFramesKeepsMetricsPath(t *testing.T) {
	sender, receiverConn := socketpairConns(t)
	consumer := &recordingConsumer{}
	r := &GpuMetricsReceiver{log: zap.NewNop(), metricsConsumer: consumer, updateServer: &exporterupdate.Server{}}
	done := make(chan struct{})
	go func() {
		r.serveFrames(receiverConn, 7)
		close(done)
	}()
	m := gpumetrics.NewGpuMetrics()
	m.Source.InstanceID = "metrics-not-update"
	data, err := m.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.SendData(data); err != nil {
		t.Fatal(err)
	}
	if err := sender.SendData(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("metrics dispatch did not finish")
	}
	if consumer.count() != 1 || consumer.received[0].Source.InstanceID != "metrics-not-update" {
		t.Fatalf("metrics consumer got %+v", consumer.received)
	}
}

func TestRunRejectsIncompleteUpdateConfigBeforeListen(t *testing.T) {
	for _, config := range []GpuMetricsReceiverConfig{
		{ExporterDebPath: "/tmp/exporter.deb"},
		{ExporterVersion: "2026.11.24-1"},
	} {
		r := &GpuMetricsReceiver{log: zap.NewNop(), config: config}
		if err := r.Run(context.Background()); err == nil {
			t.Fatalf("Run accepted incomplete config %+v", config)
		}
	}
}
