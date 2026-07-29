package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetrics"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetricsreceiver"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// gpuMetricsConsumer is the default GpuMeticsConsumer: it simply logs each
// received metrics payload. Real consumers are plugged in here.
type gpuMetricsConsumer struct {
	log *zap.Logger
}

// OnGpuMetricsReceived implements gpumetricsreceiver.GpuMeticsConsumer by
// logging the decoded payload.
func (c *gpuMetricsConsumer) OnGpuMetricsReceived(metrics *gpumetrics.GpuMetrics) error {
	c.log.Info("metrics", zap.Any("metrics", metrics))
	return nil
}

// version is the receiver build version, injected at build time via
// -ldflags "-X main.version=…". It is surfaced via the -version flag.
var version string

// main is the gpu-metrics-receiver entry point: it parses flags, sets up
// logging, and runs the receiver until the process receives SIGINT/SIGTERM.
func main() {
	var listenPort int

	flag.IntVar(&listenPort, "listenPort", 1234, "listen vsock port")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("version: %s\n", version)
		os.Exit(0)
	}

	logCfg := zap.NewProductionConfig()
	logCfg.OutputPaths = []string{"stdout"}
	logCfg.EncoderConfig.CallerKey = ""
	logCfg.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	logCfg.DisableStacktrace = true

	log, err := logCfg.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &gpuMetricsConsumer{log: log.With(zap.String("component", "gpu-metrics-consumer"))}
	gpuMetricsReceiver := gpumetricsreceiver.NewGpuMetricsReceiver(gpumetricsreceiver.GpuMetricsReceiverConfig{ListenPort: listenPort, Log: log, MetricsConsumer: c})

	err = gpuMetricsReceiver.Run(ctx)
	if err != nil {
		log.Error("Run", zap.Error(err))
	}

}
