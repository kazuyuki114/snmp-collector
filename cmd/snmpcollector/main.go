// Command snmpcollector is the main SNMP Collector binary.
//
// It loads YAML configuration from directories specified by environment
// variables (or command-line flags), builds the full pipeline, and runs until
// interrupted (SIGINT / SIGTERM).
//
// Usage:
//
//	snmpcollector [flags]
//
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"snmp/snmp-collector/pkg/snmpcollector/app"
	"snmp/snmp-collector/pkg/snmpcollector/config"
	"snmp/snmp-collector/pkg/snmpcollector/poller"
	filetransport "snmp/snmp-collector/plugin/transport/file"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "snmpcollector: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ── Flags ────────────────────────────────────────────────────────────
	var (
		logLevel  string
		logFmt    string
		collID    string
		pretty    bool
		workers   int
		bufSize   int
		enumOn    bool
		counterOn bool

		// Pool
		poolMaxIdle int
		poolIdleSec int

		// Output file transport.
		outputFile       string
		outputMaxBytes   int64
		outputMaxBackups int

		// Config path overrides (defaults read from env).
		cfgDevices      string
		cfgDeviceGroups string
		cfgObjectGroups string
		cfgObjects      string
		cfgEnums        string
	)

	flag.StringVar(&logLevel, "log.level", "info", "Log level: debug, info, warn, error")
	flag.StringVar(&logFmt, "log.fmt", "json", "Log format: json, text")
	flag.StringVar(&collID, "collector.id", "", "Collector instance ID (default: hostname)")
	flag.BoolVar(&pretty, "format.pretty", false, "Pretty-print JSON output")
	flag.IntVar(&workers, "poller.workers", 500, "Number of concurrent poller workers")
	flag.IntVar(&bufSize, "pipeline.buffer.size", 10000, "Inter-stage channel buffer size")
	flag.BoolVar(&enumOn, "processor.enum.enable", false, "Enable enum resolution")
	flag.BoolVar(&counterOn, "processor.counter.delta", true, "Enable counter delta computation")
	flag.IntVar(&poolMaxIdle, "snmp.pool.max.idle", 2, "Max idle connections per device")
	flag.IntVar(&poolIdleSec, "snmp.pool.idle.timeout", 30, "Idle connection timeout in seconds")

	flag.StringVar(&outputFile, "output.file", "", "Write metrics to file instead of stdout (enables rotation)")
	flag.Int64Var(&outputMaxBytes, "output.file.max-bytes", 50*1024*1024, "Rotate file when it exceeds this size in bytes (default 50MB)")
	flag.IntVar(&outputMaxBackups, "output.file.max-backups", 5, "Number of rotated backup files to keep (0=keep all)")

	flag.StringVar(&cfgDevices, "config.devices", "", "Override INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&cfgDeviceGroups, "config.device.groups", "", "Override INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&cfgObjectGroups, "config.object.groups", "", "Override INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&cfgObjects, "config.objects", "", "Override INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&cfgEnums, "config.enums", "", "Override PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH")

	flag.Parse()

	// ── Logger ───────────────────────────────────────────────────────────
	logger, err := buildLogger(logLevel, logFmt)
	if err != nil {
		return err
	}

	// ── Config paths ─────────────────────────────────────────────────────
	paths := config.PathsFromEnv()
	applyPathOverrides(&paths, cfgDevices, cfgDeviceGroups, cfgObjectGroups, cfgObjects, cfgEnums)

	// ── Output writer ────────────────────────────────────────────────────
	var transportCloser func()
	var transportWriter io.Writer

	if outputFile != "" {
		rf, err := filetransport.NewRotatingFile(filetransport.RotateConfig{
			FilePath:   outputFile,
			MaxBytes:   outputMaxBytes,
			MaxBackups: outputMaxBackups,
		}, logger)
		if err != nil {
			return fmt.Errorf("output file: %w", err)
		}
		transportWriter = rf
		transportCloser = func() { rf.Close() }
		logger.Info("output: writing metrics to file",
			"path", outputFile,
			"max_bytes", outputMaxBytes,
			"max_backups", outputMaxBackups,
		)
	}

	// ── Build App ────────────────────────────────────────────────────────
	cfg := app.Config{
		ConfigPaths:         paths,
		CollectorID:         collID,
		PollerWorkers:       workers,
		BufferSize:          bufSize,
		EnumEnabled:         enumOn,
		CounterDeltaEnabled: counterOn,
		PrettyPrint:         pretty,
		PoolOptions: poller.PoolOptions{
			MaxIdlePerDevice: poolMaxIdle,
			IdleTimeout:      secondsToDuration(poolIdleSec),
		},
		TransportWriter: transportWriter,
	}

	application := app.New(cfg, logger)

	// ── Start ────────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := application.Start(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	logger.Info("snmpcollector: running — press Ctrl-C to stop")

	// Block until signal.
	<-ctx.Done()
	logger.Info("snmpcollector: received shutdown signal")

	application.Stop()
	if transportCloser != nil {
		transportCloser()
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func buildLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q (expected debug|info|warn|error)", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler

	switch format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q (expected json|text)", format)
	}

	return slog.New(handler), nil
}

func applyPathOverrides(p *config.Paths, devices, dgroups, ogroups, objects, enums string) {
	if devices != "" {
		p.Devices = devices
	}
	if dgroups != "" {
		p.DeviceGroups = dgroups
	}
	if ogroups != "" {
		p.ObjectGroups = ogroups
	}
	if objects != "" {
		p.Objects = objects
	}
	if enums != "" {
		p.Enums = enums
	}
}

func secondsToDuration(sec int) time.Duration {
	return time.Duration(sec) * time.Second
}
