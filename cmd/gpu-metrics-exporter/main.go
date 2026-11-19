package main

import (
	"context"
	"errors"
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
	"go.mws.cloud/gpu-metrics-exporter/internal/selfupdate"
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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
		case "auto-update":
			os.Exit(runAutoUpdate(os.Args[2:]))
		}
	}

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
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		_, _ = fmt.Fprintf(out, "Usage:\n  %s [flags]                               run the exporter\n", os.Args[0])
		_, _ = fmt.Fprintf(out, "  %s update [-check] [-force]              upgrade the installed package to the latest release\n", os.Args[0])
		_, _ = fmt.Fprintf(out, "  %s auto-update [enable|disable|status]   check for one every 8 hours, or stop doing so\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// A stray argument would otherwise be ignored and the exporter started:
	// a second one, in the foreground, next to the service. That is what
	// releases up to 2026.10.06-1 do with `update`.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q: the exporter takes only flags, and a command (update, auto-update) must be the first argument\n", flag.Arg(0))
		flag.Usage()
		os.Exit(2)
	}

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

// runUpdate is `gpu-metrics-exporter update`: upgrade the installed package
// to the latest GitHub release (internal/selfupdate). It returns the exit code.
func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists; change nothing (needs no root)")
	force := fs.Bool("force", false, "install the latest release even when it is not newer than the installed one, "+
		"or while the service is stopped (it is left stopped)")
	auto := fs.Bool("auto", false, fmt.Sprintf("run as auto-update's timer does: leave a stopped service alone, "+
		"and exit %d when the update failed without changing anything", selfupdate.ExitTempFail))
	fs.Usage = func() {
		out := fs.Output()
		_, _ = fmt.Fprintf(out, "Usage: %s update [-check] [-force]\n\n", os.Args[0])
		_, _ = fmt.Fprintf(out, "Upgrade the installed gpu-metrics-exporter package to the latest release on\n"+
			"github.com/%s, keeping its service enabled or disabled, running or stopped,\n"+
			"as it was. Not for Kubernetes: there the DaemonSet's image is updated instead.\n\n", selfupdate.DefaultRepo)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "update: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	u := selfupdate.New(version)
	u.CheckOnly, u.Force, u.Auto = *check, *force, *auto
	err := u.Update(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
	}
	return updateExitCode(err, *auto)
}

// updateExitCode is update's exit status. An automatic run that failed
// before dpkg -i changed nothing, and exits selfupdate.ExitTempFail: the
// next run tries again, and the update unit (SuccessExitStatus=75) does not
// show as failed on a VM that cannot reach GitHub.
func updateExitCode(err error, auto bool) int {
	var installErr *selfupdate.InstallError
	switch {
	case err == nil:
		return 0
	case auto && !errors.As(err, &installErr):
		return selfupdate.ExitTempFail
	default:
		return 1
	}
}

// runAutoUpdate is `gpu-metrics-exporter auto-update [enable|disable|status]`:
// turn the timer that runs `update -auto` on or off, or report on it.
func runAutoUpdate(args []string) int {
	usage := func() {
		fmt.Fprintf(os.Stderr, "Usage: %s auto-update [enable|disable|status]\n\n"+
			"Auto-update checks for a new release every 8 hours and installs it as update\n"+
			"does; it is on unless turned off. enable and disable need root; status, the\n"+
			"default, does not. Not for Kubernetes: there the DaemonSet's image is updated.\n", os.Args[0])
	}
	cmd := "status"
	switch {
	case len(args) == 1:
		cmd = args[0]
	case len(args) > 1:
		usage()
		return 2
	}

	u := selfupdate.New(version)
	var err error
	switch cmd {
	case "enable":
		err = u.SetAutoUpdate(true)
	case "disable":
		err = u.SetAutoUpdate(false)
	case "status":
		err = u.AutoUpdateStatus()
	case "-h", "-help", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "auto-update: unknown command %q\n", cmd)
		usage()
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "auto-update: %v\n", err)
		return 1
	}
	return 0
}
