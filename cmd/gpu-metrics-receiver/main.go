package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"go.mws.cloud/gpu-metrics-exporter/internal/log"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetricsreceiver"
)

var version string

const (
	exporterDebPathEnv = "GPU_METRICS_RECEIVER_EXPORTER_DEB"
	exporterVersionEnv = "GPU_METRICS_RECEIVER_EXPORTER_VERSION"
)

func exporterUpdateConfig(getenv func(string) string) (string, string, error) {
	path := getenv(exporterDebPathEnv)
	version := getenv(exporterVersionEnv)
	if (path == "") != (version == "") {
		return "", "", fmt.Errorf("%s and %s must be set together", exporterDebPathEnv, exporterVersionEnv)
	}
	return path, version, nil
}

func main() {
	port := flag.Int("listenPort", 1234, "listen vsock port")
	logLevel := zap.LevelFlag("logLevel", zap.InfoLevel, "log level (default info)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("version: %s\n", version)
		os.Exit(0)
	}

	debPath, debVersion, err := exporterUpdateConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	logger, err := log.NewLogger(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := run(logger, *port, debPath, debVersion); err != nil {
		logger.Fatal("Run", zap.Error(err))
	}
}

func run(logger *zap.Logger, port int, debPath, debVersion string) error {
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return gpumetricsreceiver.NewGpuMetricsReceiver(gpumetricsreceiver.GpuMetricsReceiverConfig{
		ListenPort:      port,
		Log:             logger,
		MetricsConsumer: gpumetricsreceiver.NewGpuMetricsLogConsumer(logger),
		ExporterDebPath: debPath,
		ExporterVersion: debVersion,
	}).Run(ctx)
}
