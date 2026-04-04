// Package app wires the SNMP Collector pipeline stages together and manages
// their lifecycle.
//
// Pipeline:
//
//	Scheduler → WorkerPool → [rawCh] → Decoder → [decodedCh] →
//	Producer → [metricCh] → Formatter → [formattedCh] → Transport
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	jsonformat "snmp/snmp-collector/format/json"
	"snmp/snmp-collector/models"
	"snmp/snmp-collector/pkg/snmpcollector/config"
	"snmp/snmp-collector/pkg/snmpcollector/poller"
	"snmp/snmp-collector/pkg/snmpcollector/scheduler"
	"snmp/snmp-collector/producer/metrics"
	"snmp/snmp-collector/snmp/decoder"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────────────

// Config holds the top-level settings for the collector application.
// Zero-value fields fall back to documented defaults.
type Config struct {
	// ConfigPaths are the directories for YAML configuration files.
	// Use config.PathsFromEnv() to populate from environment variables.
	ConfigPaths config.Paths

	// CollectorID identifies this collector instance in output metadata.
	// Typically the hostname or pod name.
	CollectorID string

	// PollerWorkers is the number of concurrent poller goroutines.
	// Default: 500.
	PollerWorkers int

	// BufferSize is the capacity of each inter-stage channel.
	// Default: 10000.
	BufferSize int

	// PoolOptions configures the SNMP connection pool.
	PoolOptions poller.PoolOptions

	// EnumEnabled mirrors PROCESSOR_SNMP_ENUM_ENABLE.
	EnumEnabled bool

	// CounterDeltaEnabled controls counter delta computation for Counter32/64.
	CounterDeltaEnabled bool

	// PrettyPrint enables indented JSON output.
	PrettyPrint bool

	// TransportWriter is the io.Writer for file transport. nil = os.Stdout.
	TransportWriter io.Writer
}

func (c *Config) withDefaults() {
	if c.CollectorID == "" {
		name, _ := os.Hostname()
		if name == "" {
			name = "snmpcollector"
		}
		c.CollectorID = name
	}
	if c.PollerWorkers <= 0 {
		c.PollerWorkers = 500
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 10_000
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// App
// ─────────────────────────────────────────────────────────────────────────────

// App orchestrates the full SNMP collector pipeline. Create one with New,
// start it with Start, and stop it with Stop (or cancel the context).
type App struct {
	cfg    Config
	logger *slog.Logger

	// Loaded configuration (populated in Start).
	loadedCfg *config.LoadedConfig

	// Pipeline components.
	connPool   *poller.ConnectionPool
	snmpPoller *poller.SNMPPoller
	workerPool *poller.WorkerPool
	sched      *scheduler.Scheduler
	dec        *decoder.SNMPDecoder
	prod       *metrics.MetricsProducer
	formatter  *jsonformat.JSONFormatter
	transport  *writerTransport

	// Inter-stage channels.
	rawCh       chan decoder.RawPollResult
	decodedCh   chan decoder.DecodedPollResult
	metricCh    chan models.SNMPMetric
	formattedCh chan []byte

	// Lifecycle.
	cancel context.CancelFunc
	wg     sync.WaitGroup // tracks pipeline goroutines
}

// New constructs an App. It does not start anything — call Start for that.
func New(cfg Config, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	cfg.withDefaults()
	return &App{
		cfg:    cfg,
		logger: logger,
	}
}

// Start loads configuration, constructs all pipeline stages, and launches the
// goroutines that connect them. It returns an error if configuration loading
// fails.
//
// The caller must eventually call Stop (or cancel the passed-in context's
// parent) to release resources.
func (a *App) Start(ctx context.Context) error {
	// ── 1. Load configuration ───────────────────────────────────────────
	a.logger.Info("app: loading configuration")
	loadedCfg, err := config.Load(a.cfg.ConfigPaths, a.logger)
	if err != nil {
		return fmt.Errorf("app: load config: %w", err)
	}
	a.loadedCfg = loadedCfg
	a.logger.Info("app: configuration loaded",
		"devices", len(loadedCfg.Devices),
		"object_defs", len(loadedCfg.ObjectDefs),
	)

	// ── 2. Create inter-stage channels ──────────────────────────────────
	a.rawCh = make(chan decoder.RawPollResult, a.cfg.BufferSize)
	a.decodedCh = make(chan decoder.DecodedPollResult, a.cfg.BufferSize)
	a.metricCh = make(chan models.SNMPMetric, a.cfg.BufferSize)
	a.formattedCh = make(chan []byte, a.cfg.BufferSize)

	// ── 3. Build pipeline components (reverse order: transport → decoder) ──
	a.transport = newWriterTransport(a.cfg.TransportWriter, a.logger)

	a.formatter = jsonformat.New(jsonformat.Config{
		PrettyPrint: a.cfg.PrettyPrint,
	}, a.logger)

	a.prod = metrics.New(metrics.Config{
		CollectorID:         a.cfg.CollectorID,
		EnumEnabled:         a.cfg.EnumEnabled,
		Enums:               loadedCfg.Enums,
		CounterDeltaEnabled: a.cfg.CounterDeltaEnabled,
	}, a.logger)

	a.dec = decoder.NewSNMPDecoder(a.logger)

	a.connPool = poller.NewConnectionPool(a.cfg.PoolOptions, a.logger)
	a.snmpPoller = poller.NewSNMPPoller(a.connPool, a.logger)
	a.workerPool = poller.NewWorkerPool(a.cfg.PollerWorkers, a.snmpPoller, a.rawCh, a.logger)

	a.sched = scheduler.New(loadedCfg, a.workerPool, a.logger)

	// ── 4. Create a cancellable context for all goroutines ──────────────
	pipeCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel

	// ── 5. Start pipeline goroutines (transport first, sources last) ─────
	a.startTransportStage(pipeCtx)
	a.startFormatStage(pipeCtx)
	a.startProduceStage(pipeCtx)
	a.startDecodeStage(pipeCtx)

	// ── 6. Start poller path ────────────────────────────────────────────
	a.workerPool.Start(pipeCtx)

	// Scheduler blocks in its own goroutine.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.sched.Start(pipeCtx)
	}()
	a.logger.Info("app: scheduler started", "entries", a.sched.Entries())

	a.logger.Info("app: pipeline running",
		"poller_workers", a.cfg.PollerWorkers,
		"buffer_size", a.cfg.BufferSize,
	)
	return nil
}

// Stop performs a graceful shutdown.
//
// Shutdown order:
//  1. Cancel the pipeline context (stops scheduler + worker pool producers).
//  2. Wait for the scheduler goroutine to exit.
//  3. Drain the worker pool (waits for in-flight polls to complete).
//  4. Close rawCh → decoder drains → closes decodedCh → producer drains →
//     closes metricCh → formatter drains → closes formattedCh.
//  5. Transport goroutine drains formattedCh → exits.
//  6. Close transport and connection pool.
func (a *App) Stop() {
	a.logger.Info("app: shutting down")

	// 1. Signal all goroutines to stop.
	if a.cancel != nil {
		a.cancel()
	}

	// 2. Wait for the scheduler to return.
	if a.sched != nil {
		a.sched.Stop()
	}

	// 3. Drain the worker pool (waits for in-flight polls).
	if a.workerPool != nil {
		a.workerPool.Stop()
	}

	// 4. Close rawCh to cascade channel closes through the pipeline.
	if a.rawCh != nil {
		close(a.rawCh)
	}

	// 5. Wait for all pipeline goroutines to drain.
	a.wg.Wait()

	// 6. Release resources.
	if a.transport != nil {
		if err := a.transport.Close(); err != nil {
			a.logger.Error("app: transport close error", "error", err.Error())
		}
	}
	if a.connPool != nil {
		a.connPool.Close()
	}

	a.logger.Info("app: shutdown complete")
}

// Reload atomically replaces the running configuration. New devices are polled
// immediately; removed devices stop; changed intervals take effect on the next
// cycle. Returns an error if the new configuration fails to load.
func (a *App) Reload() error {
	a.logger.Info("app: reloading configuration")
	newCfg, err := config.Load(a.cfg.ConfigPaths, a.logger)
	if err != nil {
		return fmt.Errorf("app: reload config: %w", err)
	}

	// Update producer enum registry if enums changed.
	// (producer.MetricsProducer is rebuilt on the next Produce call automatically
	// via its config.Enums field — but the producer is immutable, so we just
	// update the scheduler which controls what gets polled.)
	a.sched.Reload(newCfg)
	a.loadedCfg = newCfg

	a.logger.Info("app: configuration reloaded",
		"devices", len(newCfg.Devices),
		"object_defs", len(newCfg.ObjectDefs),
	)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Pipeline stage goroutines
// ─────────────────────────────────────────────────────────────────────────────

// startDecodeStage reads RawPollResult from rawCh, decodes each into a
// DecodedPollResult, and sends it to decodedCh. When rawCh is closed (shutdown)
// it closes decodedCh to cascade the shutdown downstream.
func (a *App) startDecodeStage(_ context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(a.decodedCh)

		for raw := range a.rawCh {
			decoded, err := a.dec.Decode(raw)
			if err != nil {
				a.logger.Warn("app: decode error",
					"device", raw.Device.Hostname,
					"object", raw.ObjectDef.Key,
					"error", err.Error(),
				)
				continue
			}
			if len(decoded.Varbinds) == 0 {
				continue
			}
			a.decodedCh <- decoded
		}
	}()
}

// startProduceStage reads DecodedPollResult from decodedCh, produces an
// SNMPMetric, and sends it to metricCh. Closes metricCh when done.
func (a *App) startProduceStage(_ context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(a.metricCh)

		for decoded := range a.decodedCh {
			metric, err := a.prod.Produce(decoded)
			if err != nil {
				a.logger.Warn("app: produce error",
					"device", decoded.Device.Hostname,
					"object", decoded.ObjectDefKey,
					"error", err.Error(),
				)
				continue
			}
			if len(metric.Metrics) == 0 {
				continue
			}
			a.metricCh <- metric
		}
	}()
}

// startFormatStage reads SNMPMetric from metricCh, formats to JSON, and sends
// to formattedCh. When metricCh is closed it closes formattedCh to cascade
// shutdown to the transport stage.
func (a *App) startFormatStage(_ context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(a.formattedCh)

		for metric := range a.metricCh {
			data, err := a.formatter.Format(&metric)
			if err != nil {
				a.logger.Warn("app: format error",
					"device", metric.Device.Hostname,
					"error", err.Error(),
				)
				continue
			}
			a.formattedCh <- data
		}
	}()
}

// startTransportStage reads formatted bytes from formattedCh and writes them
// via the transport.
func (a *App) startTransportStage(_ context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		for data := range a.formattedCh {
			if err := a.transport.Send(data); err != nil {
				a.logger.Error("app: transport send error",
					"error", err.Error(),
					"bytes", len(data),
				)
			}
		}
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
// Utilities
// ─────────────────────────────────────────────────────────────────────────────

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// writerTransport is a minimal inline transport that writes each message
// followed by a newline to an io.Writer (default: os.Stdout).  It keeps
// app.go self-contained — no dependency on transport/file or plugin packages.
type writerTransport struct {
	mu     sync.Mutex
	w      io.Writer
	nl     []byte
	logger *slog.Logger
}

func newWriterTransport(w io.Writer, logger *slog.Logger) *writerTransport {
	if w == nil {
		w = os.Stdout
	}
	return &writerTransport{
		w:      w,
		nl:     []byte("\n"),
		logger: logger,
	}
}

func (t *writerTransport) Send(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, err := t.w.Write(data); err != nil {
		t.logger.Error("app: transport write failed", "error", err.Error(), "bytes", len(data))
		return fmt.Errorf("app: transport write: %w", err)
	}
	if _, err := t.w.Write(t.nl); err != nil {
		t.logger.Error("app: transport newline write failed", "error", err.Error())
		return fmt.Errorf("app: transport write newline: %w", err)
	}
	return nil
}

func (t *writerTransport) Close() error { return nil }
