// Package app wires the SNMP Collector pipeline stages together and manages
// their lifecycle.
//
// Pipeline:
//
//	Scheduler → WorkerPool → [rawCh] → Decoder (N) → [decodedCh] →
//	Producer (N) → [metricCh] → Formatter → [formattedCh] → Transport
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	jsonformat "snmp/snmp-collector/format/json"
	"snmp/snmp-collector/internal/noop"
	"snmp/snmp-collector/models"
	"snmp/snmp-collector/pkg/snmpcollector/aggregator"
	"snmp/snmp-collector/pkg/snmpcollector/config"
	"snmp/snmp-collector/pkg/snmpcollector/poller"
	"snmp/snmp-collector/pkg/snmpcollector/scheduler"
	"snmp/snmp-collector/plugin"
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

	// Transport is the output destination. It must implement plugin.Transport.
	// nil defaults to a stdout transport.
	Transport plugin.Transport

	// DecodeWorkers is the number of parallel decode-stage goroutines.
	// Default: 1.
	DecodeWorkers int

	// ProduceWorkers is the number of parallel produce-stage goroutines.
	// Default: 1.
	ProduceWorkers int

	// CounterPurgeInterval controls how often stale counter entries are
	// garbage-collected. 0 disables automatic purging.
	// Default: 5 minutes.
	CounterPurgeInterval time.Duration

	// ConfigReloadInterval, when > 0, re-reads all YAML configuration
	// directories and updates the scheduler at this interval. 0 disables
	// automatic reload.
	ConfigReloadInterval time.Duration

	// AggregatorWindow is the maximum time to wait for all objects in a device
	// poll cycle before emitting a partial summary record. Defaults to 60s.
	AggregatorWindow time.Duration
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
	if c.DecodeWorkers <= 0 {
		c.DecodeWorkers = 1
	}
	if c.ProduceWorkers <= 0 {
		c.ProduceWorkers = 1
	}
	if c.CounterPurgeInterval == 0 {
		c.CounterPurgeInterval = 5 * time.Minute
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
	formatter  jsonformat.Formatter
	transport  plugin.Transport

	// Inter-stage channels.
	rawCh        chan decoder.RawPollResult
	decodedCh    chan decoder.DecodedPollResult
	metricCh     chan models.SNMPMetric
	aggregatedCh chan models.SNMPMetric // post-aggregator, feeds formatter
	formattedCh  chan []byte

	// Lifecycle.
	cancel context.CancelFunc
	wg     sync.WaitGroup // tracks pipeline goroutines
}

// New constructs an App. It does not start anything — call Start for that.
func New(cfg Config, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noop.Writer{}, nil))
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
	a.aggregatedCh = make(chan models.SNMPMetric, a.cfg.BufferSize)
	a.formattedCh = make(chan []byte, a.cfg.BufferSize)

	// ── 3. Build pipeline components (transport first so it is ready) ───
	if a.cfg.Transport != nil {
		a.transport = a.cfg.Transport
	} else {
		a.transport = newWriterTransport(os.Stdout, a.logger)
	}

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
	a.startAggregateStage(pipeCtx)
	a.startProduceStage(pipeCtx)
	a.startDecodeStage(pipeCtx)

	// ── 6. Start optional background maintenance goroutines ─────────────
	if a.cfg.ConfigReloadInterval > 0 {
		a.startConfigReloader(pipeCtx)
	}
	if a.cfg.CounterPurgeInterval > 0 {
		a.startCounterPurger(pipeCtx)
	}

	// ── 7. Start poller path ────────────────────────────────────────────
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
		"decode_workers", a.cfg.DecodeWorkers,
		"produce_workers", a.cfg.ProduceWorkers,
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

// startDecodeStage launches DecodeWorkers goroutines that each read
// RawPollResult from rawCh and write DecodedPollResult to decodedCh.
// A coordinator goroutine closes decodedCh once all workers have exited.
func (a *App) startDecodeStage(_ context.Context) {
	var stageWg sync.WaitGroup
	for range a.cfg.DecodeWorkers {
		stageWg.Add(1)
		a.wg.Add(1)
		go func() {
			defer stageWg.Done()
			defer a.wg.Done()
			for raw := range a.rawCh {
				decoded, err := a.dec.Decode(raw)
				if err != nil {
					a.logger.Warn("app: decode error",
						"device", raw.Device.Hostname,
						"object", raw.ObjectDef.Key,
						"decoded_count", len(decoded.Varbinds),
						"error", err.Error(),
					)
					// Keep partial decode results so one malformed varbind does not
					// drop all metrics for the object (common with large interface tables).
					if len(decoded.Varbinds) == 0 {
						continue
					}
				}
				// Only drop empty results from successful polls — failure records
				// must pass through so the aggregator can count them.
				if len(decoded.Varbinds) == 0 && decoded.PollStatus == "success" {
					continue
				}
				a.decodedCh <- decoded
			}
		}()
	}
	// Closer: waits for all decode workers then cascades shutdown downstream.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		stageWg.Wait()
		close(a.decodedCh)
	}()
}

// startProduceStage launches ProduceWorkers goroutines that each read
// DecodedPollResult from decodedCh and write SNMPMetric to metricCh.
// A coordinator goroutine closes metricCh once all workers have exited.
func (a *App) startProduceStage(_ context.Context) {
	var stageWg sync.WaitGroup
	for range a.cfg.ProduceWorkers {
		stageWg.Add(1)
		a.wg.Add(1)
		go func() {
			defer stageWg.Done()
			defer a.wg.Done()
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
				// Only drop zero-metric results from successful polls — failure
				// records (metrics==[], poll_status==error) must reach the aggregator.
				if len(metric.Metrics) == 0 && metric.Metadata.PollStatus == "success" {
					continue
				}
				a.metricCh <- metric
			}
		}()
	}
	// Closer: waits for all produce workers then cascades shutdown downstream.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		stageWg.Wait()
		close(a.metricCh)
	}()
}

// startAggregateStage reads SNMPMetric from metricCh, passes each record
// through to aggregatedCh unchanged, and emits a per-device summary record
// once all objects for a poll cycle have been accounted for.
// Closes aggregatedCh when metricCh is drained.
func (a *App) startAggregateStage(ctx context.Context) {
	agg := aggregator.New(
		aggregator.Config{
			ObjectCounts: a.sched.ObjectCounts(),
			Window:       a.cfg.AggregatorWindow,
			CollectorID:  a.cfg.CollectorID,
		},
		a.metricCh,
		a.aggregatedCh,
		a.logger,
	)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(a.aggregatedCh)
		agg.Run(ctx)
	}()
}

// startFormatStage reads SNMPMetric from aggregatedCh, formats to JSON, and
// sends to formattedCh. Closes formattedCh when aggregatedCh is drained.
func (a *App) startFormatStage(_ context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(a.formattedCh)

		for metric := range a.aggregatedCh {
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

// startConfigReloader launches a goroutine that calls Reload on every tick of
// cfg.ConfigReloadInterval. It stops when ctx is cancelled.
func (a *App) startConfigReloader(ctx context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(a.cfg.ConfigReloadInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.Reload(); err != nil {
					a.logger.Error("app: scheduled config reload failed", "error", err.Error())
				}
			}
		}
	}()
	a.logger.Info("app: automatic config reload enabled",
		"interval", a.cfg.ConfigReloadInterval,
	)
}

// startCounterPurger launches a goroutine that periodically removes stale
// counter entries from the producer's CounterState to prevent unbounded memory
// growth in long-running deployments.
func (a *App) startCounterPurger(ctx context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(a.cfg.CounterPurgeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Entries not seen for 3× the purge interval are considered stale.
				maxAge := 3 * a.cfg.CounterPurgeInterval
				if n := a.prod.PurgeCounters(maxAge); n > 0 {
					a.logger.Info("app: purged stale counter entries",
						"removed", n,
						"max_age", maxAge,
					)
				}
			}
		}
	}()
}

// ─────────────────────────────────────────────────────────────────────────────
// Default transport
// ─────────────────────────────────────────────────────────────────────────────

// writerTransport is the default plugin.Transport used when no Transport is
// provided in Config. It writes each message followed by a newline to an
// io.Writer (default: os.Stdout).
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

func (t *writerTransport) Name() string { return "stdout" }

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
