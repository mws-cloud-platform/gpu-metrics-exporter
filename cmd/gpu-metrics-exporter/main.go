package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"go.mws.cloud/gpu-metrics-exporter/internal/log"
	"go.mws.cloud/gpu-metrics-exporter/pkg/gpumetricsexporter"
)

var version string

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func main() {
	defaultPort := getEnvInt("SERVER_PORT", 9999)
	defaultTick := getEnvInt("TICK_PERIOD", 60)
	defaultLogLevel := getEnv("LOG_LEVEL", "info")
	defaultNvmlPath := getEnv("NVML_LIB_PATH", "")
	defaultInstanceIDPath := getEnv("INSTANCE_ID_PATH", "/var/lib/cloud/data/instance-id")
	defaultHostRoot := getEnv("HOST_ROOT", "")
	if defaultHostRoot == "" {
		if fi, err := os.Stat("/host"); err == nil && fi.IsDir() {
			defaultHostRoot = "/host"
		} else {
			defaultHostRoot = "/"
		}
	}

	port := flag.Int("serverPort", defaultPort, "server vsock port (env: SERVER_PORT)")
	periodSecs := flag.Int("tickPeriod", defaultTick, "metrics gathering period in seconds (env: TICK_PERIOD)")
	logLevelStr := flag.String("logLevel", defaultLogLevel, "log level: debug, info, warn, error (env: LOG_LEVEL)")
	nvmlLibPath := flag.String("nvmlLibPath", defaultNvmlPath, "path to libnvidia-ml.so (env: NVML_LIB_PATH)")
	instanceIdPath := flag.String("instanceIdPath", defaultInstanceIDPath, "path to cloud-init instance-id file (env: INSTANCE_ID_PATH)")
	hostRoot := flag.String("hostRoot", defaultHostRoot, "path to host root filesystem mount (env: HOST_ROOT)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("version: %s\n", version)
		os.Exit(0)
	}

	var level zapcore.Level
	if err := level.UnmarshalText([]byte(*logLevelStr)); err != nil {
		fmt.Fprintf(os.Stderr, "Invalid log level %q, falling back to info: %v\n", *logLevelStr, err)
		level = zapcore.InfoLevel
	}

	logger, err := log.NewLogger(level)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	cfg := gpumetricsexporter.GpuMetricsExporterConfig{
		ServerPort:     *port,
		Log:            logger,
		TickPeriod:     time.Duration(*periodSecs) * time.Second,
		Version:        version,
		NvmlLibPath:    *nvmlLibPath,
		InstanceIDPath: *instanceIdPath,
		HostRoot:       *hostRoot,
	}

	if err := run(logger, cfg); err != nil {
		logger.Fatal("Run", zap.Error(err))
	}
}

func run(logger *zap.Logger, cfg gpumetricsexporter.GpuMetricsExporterConfig) error {
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return gpumetricsexporter.NewGpuMetricsExporter(cfg).Run(ctx)
}
