package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	gpumetricsexporter "go.mws.cloud/gpu-metrics-exporter/pkg/gpumetricsexporter"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// version is the exporter build version, injected at build time via
// -ldflags "-X main.version=…". It is surfaced via the -version flag and
// stamped onto every emitted metrics payload.
var version string

// main is the gpu-metrics-exporter entry point: it parses flags, sets up
// logging, wires SIGINT/SIGTERM to graceful shutdown, and runs the exporter
// until stopped.
func main() {
	var serverPort int
	var tickPeriod int

	flag.IntVar(&serverPort, "serverPort", 1234, "server vsock port")
	flag.IntVar(&tickPeriod, "tickPeriod", 10, "metrics gathering tick period")
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
		fmt.Fprintf(os.Stderr, "logCfg.Build error: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	e := gpumetricsexporter.NewGpuMetricsExporter(gpumetricsexporter.GpuMetricsExporterConfig{ServerPort: serverPort, Log: log, TickPeriod: tickPeriod, Version: version})
	go func() {
		<-sigCh
		e.StopOnSignal()
	}()

	err = e.Run()
	if err != nil {
		log.Error("a.Run", zap.Error(err))
		os.Exit(1)
	}
}
