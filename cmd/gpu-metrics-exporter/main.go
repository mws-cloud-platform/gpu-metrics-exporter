package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"go.mws.cloud/gpu-metrics-exporter/internal/log"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetricsexporter"
)

var version string

func main() {
	port := flag.Int("serverPort", 1234, "server vsock port")
	periodSecs := flag.Int("tickPeriod", 10, "metrics gathering period in seconds")
	logLevel := zap.LevelFlag("logLevel", zap.InfoLevel, "log level (default info)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("version: %s\n", version)
		os.Exit(0)
	}

	logger, err := log.NewLogger(*logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := run(logger, *port, *periodSecs, version); err != nil {
		logger.Fatal("Run", zap.Error(err))
	}
}

func run(logger *zap.Logger, port int, periodSecs int, version string) error {
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return gpumetricsexporter.NewGpuMetricsExporter(gpumetricsexporter.GpuMetricsExporterConfig{
		ServerPort: port,
		Log:        logger,
		TickPeriod: time.Duration(periodSecs) * time.Second,
		Version:    version,
	}).Run(ctx)
}
