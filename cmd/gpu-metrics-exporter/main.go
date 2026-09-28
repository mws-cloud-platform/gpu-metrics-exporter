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

func getEnv(primaryKey, fallbackKey, defaultVal string) string {
	if val, ok := os.LookupEnv(primaryKey); ok && val != "" {
		return val
	}
	if fallbackKey != "" {
		if val, ok := os.LookupEnv(fallbackKey); ok && val != "" {
			return val
		}
	}
	return defaultVal
}

func getEnvInt(primaryKey, fallbackKey string, defaultVal int) (int, error) {
	valStr := ""
	keyUsed := ""
	if val, ok := os.LookupEnv(primaryKey); ok && val != "" {
		valStr = val
		keyUsed = primaryKey
	} else if fallbackKey != "" {
		if val, ok := os.LookupEnv(fallbackKey); ok && val != "" {
			valStr = val
			keyUsed = fallbackKey
		}
	}
	if valStr == "" {
		return defaultVal, nil
	}
	i, err := strconv.Atoi(valStr)
	if err != nil {
		return 0, fmt.Errorf("invalid integer value %q for environment variable %s: %w", valStr, keyUsed, err)
	}
	return i, nil
}

func main() {
	defaultPort, err := getEnvInt("GPU_METRICS_EXPORTER_SERVER_PORT", "SERVER_PORT", 1234)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}
	defaultTick, err := getEnvInt("GPU_METRICS_EXPORTER_TICK_PERIOD", "TICK_PERIOD", 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}
	defaultLogLevel := getEnv("GPU_METRICS_EXPORTER_LOG_LEVEL", "LOG_LEVEL", "info")
	defaultNvmlPath := getEnv("GPU_METRICS_EXPORTER_NVML_LIB_PATH", "NVML_LIB_PATH", "")
	defaultInstanceIDPath := getEnv("GPU_METRICS_EXPORTER_INSTANCE_ID_PATH", "INSTANCE_ID_PATH", "/var/lib/cloud/data/instance-id")
	defaultHostRoot := getEnv("GPU_METRICS_EXPORTER_HOST_ROOT", "HOST_ROOT", "/")

	port := flag.Int("serverPort", defaultPort, "server vsock port (env: GPU_METRICS_EXPORTER_SERVER_PORT, SERVER_PORT)")
	periodSecs := flag.Int("tickPeriod", defaultTick, "metrics gathering period in seconds (env: GPU_METRICS_EXPORTER_TICK_PERIOD, TICK_PERIOD)")
	logLevelStr := flag.String("logLevel", defaultLogLevel, "log level: debug, info, warn, error (env: GPU_METRICS_EXPORTER_LOG_LEVEL, LOG_LEVEL)")
	nvmlLibPath := flag.String("nvmlLibPath", defaultNvmlPath, "path to libnvidia-ml.so (env: GPU_METRICS_EXPORTER_NVML_LIB_PATH, NVML_LIB_PATH)")
	instanceIdPath := flag.String("instanceIdPath", defaultInstanceIDPath, "path to cloud-init instance-id file (env: GPU_METRICS_EXPORTER_INSTANCE_ID_PATH, INSTANCE_ID_PATH)")
	hostRoot := flag.String("hostRoot", defaultHostRoot, "path to host root filesystem mount (env: GPU_METRICS_EXPORTER_HOST_ROOT, HOST_ROOT)")
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
