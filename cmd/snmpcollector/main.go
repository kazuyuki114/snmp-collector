// Command snmpcollector is the main SNMP Collector binary.
//
// It can be configured via a YAML file, CLI flags, or both.
// When both are provided, CLI flags take precedence over the YAML file.
//
// Usage:
//
//	snmpcollector -config /etc/snmpcollector/config.yaml
//	snmpcollector -config /etc/snmpcollector/config.yaml -log.level debug
//	snmpcollector -output.kafka.brokers broker1:9092 -output.kafka.topic snmp
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"snmp/snmp-collector/internal/app"
	"snmp/snmp-collector/internal/config"
	"snmp/snmp-collector/internal/ha"
	"snmp/snmp-collector/internal/health"
	"snmp/snmp-collector/internal/plugin"
	filetransport "snmp/snmp-collector/internal/plugin/transport/file"
	kafkatransport "snmp/snmp-collector/internal/plugin/transport/kafka"
	"snmp/snmp-collector/internal/poller"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "snmpcollector: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ── Step 1: load YAML config ─────────────────────────────────────────────
	//
	// Pre-scan os.Args for -config before calling flag.Parse so we can use the
	// YAML values as defaults when registering flags.  flag.Parse then lets any
	// explicit CLI flag silently override those defaults.
	//
	//   Priority (highest → lowest):  CLI flag > YAML file > hardcoded default
	//
	cc := config.DefaultCollectorConfig()
	if cfgFile := preScanFlag(os.Args[1:], "config"); cfgFile != "" {
		loaded, err := config.LoadCollectorConfig(cfgFile)
		if err != nil {
			return err
		}
		cc = loaded
	}

	// ── Step 2: register all flags (YAML values become the defaults) ─────────
	var (
		logLevel         string
		logFmt           string
		collID           string
		pretty           bool
		otelFormat       bool
		otelScopeName    string
		otelScopeVersion string
		workers          int
		bufSize          int
		enumOn           bool
		counterOn        bool

		decodeWorkers  int
		produceWorkers int

		poolMaxIdle int
		poolIdleSec int

		outputSendMaxRetry   int
		outputSendRetryDelay string

		outputFile       string
		outputMaxBytes   int64
		outputMaxBackups int

		kafkaBrokers       string
		kafkaTopic         string
		kafkaMaxEvents     int
		kafkaFlushSec      int
		kafkaBufferSize    int
		kafkaClientID      string
		kafkaRequiredAcks  string
		kafkaMaxRetry      int
		kafkaCompression   string
		kafkaVersion       string
		kafkaTLSEnable     bool
		kafkaTLSSkipVerify bool
		kafkaTLSCACert     string
		kafkaTLSClientCert string
		kafkaTLSClientKey  string
		kafkaSASLEnable    bool
		kafkaSASLUser      string
		kafkaSASLPassword  string
		kafkaSASLMechanism string

		healthAddr string

		haEnable  bool
		haRole    string
		haPeerURL string

		cfgReloadSec    int
		counterPurgeSec int

		cfgDevices      string
		cfgDeviceGroups string
		cfgObjectGroups string
		cfgObjects      string
		cfgEnums        string

		maxProcs int
	)

	// -config is registered so it appears in -help and does not produce an
	// "unknown flag" error.  The value was already consumed by preScanFlag.
	flag.String("config", "", "Path to collector YAML config file (flags override file values)")

	flag.StringVar(&logLevel, "log.level", cc.Log.Level, "Log level: debug, info, warn, error")
	flag.StringVar(&logFmt, "log.fmt", cc.Log.Format, "Log format: json, text")
	flag.StringVar(&collID, "collector.id", cc.CollectorID, "Collector instance ID (default: hostname)")
	flag.BoolVar(&pretty, "format.pretty", cc.Format.Pretty, "Pretty-print JSON output")
	flag.BoolVar(&otelFormat, "format.otel", cc.Format.OTel, "Emit OpenTelemetry OTLP JSON instead of custom JSON (overrides format.pretty)")
	flag.StringVar(&otelScopeName, "format.otel.scope-name", cc.Format.OTelScopeName, "OTLP instrumentation scope name (default: snmp-collector)")
	flag.StringVar(&otelScopeVersion, "format.otel.scope-version", cc.Format.OTelScopeVersion, "OTLP instrumentation scope version string")
	flag.IntVar(&workers, "poller.workers", cc.Poller.Workers, "Number of concurrent poller workers")
	flag.IntVar(&bufSize, "pipeline.buffer.size", cc.Pipeline.BufferSize, "Inter-stage channel buffer size")
	flag.IntVar(&decodeWorkers, "pipeline.decode.workers", cc.Pipeline.DecodeWorkers, "Number of parallel decode-stage goroutines")
	flag.IntVar(&produceWorkers, "pipeline.produce.workers", cc.Pipeline.ProduceWorkers, "Number of parallel produce-stage goroutines")
	flag.BoolVar(&enumOn, "processor.enum.enable", cc.Processors.Enum.Enable, "Enable enum resolution")
	flag.BoolVar(&counterOn, "processor.counter.delta", cc.Processors.Counter.DeltaEnable, "Enable counter delta computation")
	flag.IntVar(&counterPurgeSec, "processor.counter.purge.interval", durToSec(cc.Processors.Counter.PurgeInterval), "Counter state GC interval in seconds (0 = disabled)")
	flag.IntVar(&poolMaxIdle, "snmp.pool.max.idle", cc.Poller.Pool.MaxIdle, "Max idle connections per device")
	flag.IntVar(&poolIdleSec, "snmp.pool.idle.timeout", durToSec(cc.Poller.Pool.IdleTimeout), "Idle connection timeout in seconds")

	flag.IntVar(&outputSendMaxRetry, "output.send.retry.max", cc.Output.SendMaxRetry, "Retries per Send failure before dropping the message (0 = no retry)")
	flag.StringVar(&outputSendRetryDelay, "output.send.retry.delay", cc.Output.SendRetryDelay, "Delay between Send retries (e.g. 1s, 500ms)")

	flag.StringVar(&outputFile, "output.file", cc.Output.File.Path, "Write metrics to file instead of stdout (enables rotation)")
	flag.Int64Var(&outputMaxBytes, "output.file.max-bytes", cc.Output.File.MaxBytes, "Rotate file when it exceeds this size in bytes (default 50MB)")
	flag.IntVar(&outputMaxBackups, "output.file.max-backups", cc.Output.File.MaxBackups, "Number of rotated backup files to keep (0=keep all)")

	flag.StringVar(&kafkaBrokers, "output.kafka.brokers", strings.Join(cc.Output.Kafka.Brokers, ","), "Comma-separated Kafka broker list (host:port); enables Kafka output")
	flag.StringVar(&kafkaTopic, "output.kafka.topic", cc.Output.Kafka.Topic, "Kafka topic to publish metrics to")
	flag.IntVar(&kafkaMaxEvents, "output.kafka.max-events", cc.Output.Kafka.MaxEvents, "Max messages per batch before forced flush")
	flag.IntVar(&kafkaFlushSec, "output.kafka.flush-interval", durToSec(cc.Output.Kafka.FlushInterval), "Max seconds a batch can age before flush")
	flag.IntVar(&kafkaBufferSize, "output.kafka.buffer-size", cc.Output.Kafka.BufferSize, "Internal channel buffer capacity")
	flag.StringVar(&kafkaClientID, "output.kafka.client-id", cc.Output.Kafka.ClientID, "Kafka client identifier")
	flag.StringVar(&kafkaRequiredAcks, "output.kafka.required-acks", cc.Output.Kafka.RequiredAcks, "Producer ACK level: none, local, all")
	flag.IntVar(&kafkaMaxRetry, "output.kafka.max-retry", cc.Output.Kafka.MaxRetry, "Number of send retries on transient errors")
	flag.StringVar(&kafkaCompression, "output.kafka.compression", cc.Output.Kafka.Compression, "Message compression: none, gzip, snappy, lz4, zstd")
	flag.StringVar(&kafkaVersion, "output.kafka.version", cc.Output.Kafka.Version, "Kafka protocol version to negotiate (e.g. 3.6.0)")
	flag.BoolVar(&kafkaTLSEnable, "output.kafka.tls.enable", cc.Output.Kafka.TLS.Enable, "Enable TLS for broker connections")
	flag.BoolVar(&kafkaTLSSkipVerify, "output.kafka.tls.insecure-skip-verify", cc.Output.Kafka.TLS.InsecureSkipVerify, "Skip broker TLS certificate verification (not for production)")
	flag.StringVar(&kafkaTLSCACert, "output.kafka.tls.ca-cert", cc.Output.Kafka.TLS.CACert, "Path to PEM CA certificate for broker verification")
	flag.StringVar(&kafkaTLSClientCert, "output.kafka.tls.client-cert", cc.Output.Kafka.TLS.ClientCert, "Path to PEM client certificate for mTLS")
	flag.StringVar(&kafkaTLSClientKey, "output.kafka.tls.client-key", cc.Output.Kafka.TLS.ClientKey, "Path to PEM client key for mTLS")
	flag.BoolVar(&kafkaSASLEnable, "output.kafka.sasl.enable", cc.Output.Kafka.SASL.Enable, "Enable SASL authentication")
	flag.StringVar(&kafkaSASLUser, "output.kafka.sasl.user", cc.Output.Kafka.SASL.User, "SASL username")
	flag.StringVar(&kafkaSASLPassword, "output.kafka.sasl.password", cc.Output.Kafka.SASL.Password, "SASL password")
	flag.StringVar(&kafkaSASLMechanism, "output.kafka.sasl.mechanism", cc.Output.Kafka.SASL.Mechanism, "SASL mechanism: PLAIN, SCRAM-SHA-256, SCRAM-SHA-512")

	flag.IntVar(&cfgReloadSec, "config.reload.interval", durToSec(cc.ConfigReloadInterval), "Re-read all config directories every N seconds and update the scheduler (0 = disabled)")

	flag.StringVar(&cfgDevices, "config.devices", cc.ConfigPaths.Devices, "Override INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&cfgDeviceGroups, "config.device.groups", cc.ConfigPaths.DeviceGroups, "Override INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&cfgObjectGroups, "config.object.groups", cc.ConfigPaths.ObjectGroups, "Override INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&cfgObjects, "config.objects", cc.ConfigPaths.Objects, "Override INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&cfgEnums, "config.enums", cc.ConfigPaths.Enums, "Override PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH")
	flag.StringVar(&healthAddr, "health.addr", cc.Health.Addr, "Address to expose /health endpoint (e.g. :8080); disabled if empty")
	flag.IntVar(&maxProcs, "runtime.gomaxprocs", cc.MaxProcs, "Number of OS threads (GOMAXPROCS); 0 = all CPUs")

	flag.BoolVar(&haEnable, "ha.enable", cc.HA.Enabled, "Enable Active/Standby HA manager")
	flag.StringVar(&haRole, "ha.role", cc.HA.Role, "HA role for this node: primary (DC) or standby (DR)")
	flag.StringVar(&haPeerURL, "ha.peer.url", cc.HA.PeerURL, "Base HTTP URL of the peer node")

	flag.Parse()

	if maxProcs > 0 {
		runtime.GOMAXPROCS(maxProcs)
	}

	// ── Step 3: build the application (identical to before) ─────────────────

	logger, err := buildLogger(logLevel, logFmt)
	if err != nil {
		return err
	}

	paths := config.PathsFromEnv()
	applyPathOverrides(&paths, cfgDevices, cfgDeviceGroups, cfgObjectGroups, cfgObjects, cfgEnums)

	cfg := app.Config{
		ConfigPaths:          paths,
		CollectorID:          collID,
		PollerWorkers:        workers,
		BufferSize:           bufSize,
		DecodeWorkers:        decodeWorkers,
		ProduceWorkers:       produceWorkers,
		EnumEnabled:          enumOn,
		CounterDeltaEnabled:  counterOn,
		CounterPurgeInterval: secondsToDuration(counterPurgeSec),
		PrettyPrint:          pretty,
		OTelFormat:           otelFormat,
		OTelScopeName:        otelScopeName,
		OTelScopeVersion:     otelScopeVersion,
		PoolOptions: poller.PoolOptions{
			MaxIdlePerDevice: poolMaxIdle,
			IdleTimeout:      secondsToDuration(poolIdleSec),
		},
		ConfigReloadInterval: secondsToDuration(cfgReloadSec),
		TransportMaxRetry:    outputSendMaxRetry,
		TransportRetryDelay:  parseDurationOrDefault(outputSendRetryDelay, time.Second),
	}

	// ── Step 4: select output transport ─────────────────────────────────────
	//
	// buildTransport is a factory closure that creates a fresh transport from
	// the resolved flags/YAML. In non-HA mode it is called once and the result
	// stored in cfg.Transport. In HA mode it is stored in cfg.TransportFactory
	// so each app.Start() (failover/failback cycle) gets a new connection —
	// calling app.Stop() closes the previous transport and it cannot be reused.
	var buildTransport func() (plugin.Transport, error)

	if preScanFlag(os.Args[1:], "config") != "" {
		enabledCount := 0
		if cc.Output.Stdout.Enabled {
			enabledCount++
		}
		if cc.Output.File.Enabled {
			enabledCount++
		}
		if cc.Output.Kafka.Enabled {
			enabledCount++
		}
		if enabledCount > 1 {
			return fmt.Errorf("output: exactly one output block must have enabled: true, but %d are enabled", enabledCount)
		}

		switch {
		case cc.Output.Kafka.Enabled:
			var tlsCfg *kafkatransport.TLSConfig
			if cc.Output.Kafka.TLS.Enable {
				tlsCfg = &kafkatransport.TLSConfig{
					Enable:             true,
					InsecureSkipVerify: cc.Output.Kafka.TLS.InsecureSkipVerify,
					CACert:             cc.Output.Kafka.TLS.CACert,
					ClientCert:         cc.Output.Kafka.TLS.ClientCert,
					ClientKey:          cc.Output.Kafka.TLS.ClientKey,
				}
			}
			var saslCfg *kafkatransport.SASLConfig
			if cc.Output.Kafka.SASL.Enable {
				saslCfg = &kafkatransport.SASLConfig{
					Enable:    true,
					User:      cc.Output.Kafka.SASL.User,
					Password:  cc.Output.Kafka.SASL.Password,
					Mechanism: cc.Output.Kafka.SASL.Mechanism,
				}
			}
			ktCfg := kafkatransport.Config{
				Brokers:       cc.Output.Kafka.Brokers,
				Topic:         cc.Output.Kafka.Topic,
				MaxEvents:     cc.Output.Kafka.MaxEvents,
				FlushInterval: time.Duration(kafkaFlushSec) * time.Second,
				BufferSize:    cc.Output.Kafka.BufferSize,
				ClientID:      cc.Output.Kafka.ClientID,
				RequiredAcks:  cc.Output.Kafka.RequiredAcks,
				MaxRetry:      cc.Output.Kafka.MaxRetry,
				Compression:   cc.Output.Kafka.Compression,
				Version:       cc.Output.Kafka.Version,
				TLS:           tlsCfg,
				SASL:          saslCfg,
			}
			buildTransport = func() (plugin.Transport, error) {
				return kafkatransport.New(ktCfg, logger)
			}

		case cc.Output.File.Enabled:
			ftCfg := filetransport.Config{
				FilePath:   cc.Output.File.Path,
				MaxBytes:   cc.Output.File.MaxBytes,
				MaxBackups: cc.Output.File.MaxBackups,
			}
			logger.Info("output: writing metrics to file",
				"path", cc.Output.File.Path,
				"max_bytes", cc.Output.File.MaxBytes,
				"max_backups", cc.Output.File.MaxBackups,
			)
			buildTransport = func() (plugin.Transport, error) {
				return filetransport.New(ftCfg, logger)
			}

		default:
			// stdout.enabled: true or no block enabled → stdout (buildTransport stays nil)
		}
	} else {
		// CLI-only mode: priority logic (kafka brokers → file path → stdout)
		switch {
		case kafkaBrokers != "":
			var tlsCfg *kafkatransport.TLSConfig
			if kafkaTLSEnable {
				tlsCfg = &kafkatransport.TLSConfig{
					Enable:             true,
					InsecureSkipVerify: kafkaTLSSkipVerify,
					CACert:             kafkaTLSCACert,
					ClientCert:         kafkaTLSClientCert,
					ClientKey:          kafkaTLSClientKey,
				}
			}
			var saslCfg *kafkatransport.SASLConfig
			if kafkaSASLEnable {
				saslCfg = &kafkatransport.SASLConfig{
					Enable:    true,
					User:      kafkaSASLUser,
					Password:  kafkaSASLPassword,
					Mechanism: kafkaSASLMechanism,
				}
			}
			ktCfg := kafkatransport.Config{
				Brokers:       strings.Split(kafkaBrokers, ","),
				Topic:         kafkaTopic,
				MaxEvents:     kafkaMaxEvents,
				FlushInterval: time.Duration(kafkaFlushSec) * time.Second,
				BufferSize:    kafkaBufferSize,
				ClientID:      kafkaClientID,
				RequiredAcks:  kafkaRequiredAcks,
				MaxRetry:      kafkaMaxRetry,
				Compression:   kafkaCompression,
				Version:       kafkaVersion,
				TLS:           tlsCfg,
				SASL:          saslCfg,
			}
			buildTransport = func() (plugin.Transport, error) {
				return kafkatransport.New(ktCfg, logger)
			}

		case outputFile != "":
			ftCfg := filetransport.Config{
				FilePath:   outputFile,
				MaxBytes:   outputMaxBytes,
				MaxBackups: outputMaxBackups,
			}
			logger.Info("output: writing metrics to file",
				"path", outputFile,
				"max_bytes", outputMaxBytes,
				"max_backups", outputMaxBackups,
			)
			buildTransport = func() (plugin.Transport, error) {
				return filetransport.New(ftCfg, logger)
			}
		}
	}

	// Wire the transport into app.Config.
	// HA mode: use TransportFactory so each Start() creates a fresh connection.
	// Non-HA mode: build once now and store in Transport (existing behaviour).
	if haEnable {
		cfg.TransportFactory = buildTransport // nil = stdout; app handles it
	} else if buildTransport != nil {
		t, err := buildTransport()
		if err != nil {
			return fmt.Errorf("output: %w", err)
		}
		cfg.Transport = t
	}

	application := app.New(cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── Step 5: wire HA manager (if enabled) ─────────────────────────────────
	//
	// stopApp is an idempotent wrapper around application.Stop so that a
	// /demote-driven stop and the SIGTERM shutdown path never both call
	// close(rawCh), which would panic.
	var stopOnce sync.Once
	stopApp := func() { stopOnce.Do(application.Stop) }

	var haMgr *ha.Manager
	if haEnable {
		if haPeerURL == "" {
			return fmt.Errorf("ha: ha.peer.url must be set when ha.enable is true")
		}
		role := ha.RoleStandby
		if haRole == "primary" {
			role = ha.RolePrimary
		}
		haMgr = ha.New(ha.Config{
			Role:                role,
			PeerURL:             haPeerURL,
			HealthCheckInterval: parseDurationOrDefault(cc.HA.HealthCheckInterval, 5*time.Second),
			HealthCheckTimeout:  parseDurationOrDefault(cc.HA.HealthCheckTimeout, 5*time.Second),
			FailoverThreshold:   cc.HA.FailoverThreshold,
			DemoteTimeout:       parseDurationOrDefault(cc.HA.DemoteTimeout, 30*time.Second),
		}, logger)
		haMgr.OnStartPolling(func() {
			if err := application.Start(ctx); err != nil {
				logger.Error("ha: failed to start polling engine", "error", err)
			}
		})
		haMgr.OnStopPolling(stopApp)
	} else {
		// Standalone mode — start the polling engine unconditionally.
		if err := application.Start(ctx); err != nil {
			return fmt.Errorf("start: %w", err)
		}
	}

	// ── Step 6: start HTTP server (health + optional /demote) ────────────────
	//
	// The server is started before the HA manager so that /demote is already
	// accepting connections when the Primary's startup demotePeer call arrives,
	// and /health is answering before the peer's health-check loop fires.
	if healthAddr != "" {
		healthSrv := health.NewServer(healthAddr, collID, logger)
		if haMgr != nil {
			healthSrv.Handle("/demote", haMgr.DemoteHandler())
		}
		healthSrv.Start()
		defer healthSrv.Stop(ctx)
	}

	// ── Step 7: start HA manager ──────────────────────────────────────────────
	if haMgr != nil {
		haMgr.Start(ctx)
	}

	logger.Info("snmpcollector: running — press Ctrl-C to stop")
	<-ctx.Done()
	logger.Info("snmpcollector: received shutdown signal")

	// ── Step 8: graceful shutdown ─────────────────────────────────────────────
	if haMgr != nil {
		// Stop the HA background loop. This does NOT fire OnStopPolling;
		// it only cancels the peer-health-check / preemption goroutine.
		haMgr.Stop()
	}
	// stopApp is idempotent: no-op if the app was already stopped by a
	// /demote-triggered OnStopPolling call during normal HA operation.
	stopApp()
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// preScanFlag scans args for "-<name> <value>", "--<name> <value>", or
// "-<name>=<value>" without going through the standard flag package.
// This is needed so the YAML file can be loaded before flag.Parse runs
// (which in turn uses the YAML values as flag defaults).
func preScanFlag(args []string, name string) string {
	for i, arg := range args {
		if arg == "-"+name || arg == "--"+name {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if v, ok := strings.CutPrefix(arg, "-"+name+"="); ok {
			return v
		}
		if v, ok := strings.CutPrefix(arg, "--"+name+"="); ok {
			return v
		}
	}
	return ""
}

// durToSec parses a duration string and returns whole seconds.
// Returns 0 on empty input or parse error (disabled).
func durToSec(s string) int {
	d, err := config.ParseDuration(s)
	if err != nil || d == 0 {
		return 0
	}
	return int(d.Seconds())
}

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

func parseDurationOrDefault(s string, def time.Duration) time.Duration {
	d, err := config.ParseDuration(s)
	if err != nil || d == 0 {
		return def
	}
	return d
}
