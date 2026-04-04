// Package snmppoller implements the SNMP Poller Input plugin.
//
// It wraps the existing scheduler, worker pool, decoder, and producer
// pipeline stages into a single [plugin.Input] so the engine can manage
// it identically to any future input (gNMI, NETCONF, etc.).
//
// Pipeline (internal to this plugin):
//
//	Scheduler → WorkerPool → [rawCh] → Decoder → [decodedCh] → Producer → Envelope
//
// The plugin owns all goroutines for these stages and shuts them down
// cleanly when Stop is called.
package snmppoller

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

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

// Config holds all settings required to initialise the SNMP Poller input.
type Config struct {
	// ConfigPaths locates the YAML configuration directories.
	ConfigPaths config.Paths

	// CollectorID identifies this collector instance in output metadata.
	CollectorID string

	// PollerWorkers is the number of concurrent poller goroutines (default 500).
	PollerWorkers int

	// BufferSize is the capacity of each internal inter-stage channel (default 10 000).
	BufferSize int

	// PoolOptions configures the SNMP connection pool.
	PoolOptions poller.PoolOptions

	// EnumEnabled enables enum resolution in the producer.
	EnumEnabled bool

	// CounterDeltaEnabled enables counter delta computation.
	CounterDeltaEnabled bool
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
// Input implementation
// ─────────────────────────────────────────────────────────────────────────────

// Input is the SNMP Poller plugin. It implements [plugin.Input].
type Input struct {
	cfg    Config
	logger *slog.Logger

	// Loaded config (populated in Start).
	loadedCfg *config.LoadedConfig

	// Internal pipeline components.
	connPool   *poller.ConnectionPool
	snmpPoller *poller.SNMPPoller
	workerPool *poller.WorkerPool
	sched      *scheduler.Scheduler
	dec        *decoder.SNMPDecoder
	prod       *metrics.MetricsProducer

	// Internal channels.
	rawCh     chan decoder.RawPollResult
	decodedCh chan decoder.DecodedPollResult

	// Lifecycle.
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// compile-time interface check.
var _ plugin.Input = (*Input)(nil)

// New creates an SNMP Poller Input. Call Start to begin polling.
func New(cfg Config, logger *slog.Logger) *Input {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	cfg.withDefaults()
	return &Input{
		cfg:    cfg,
		logger: logger,
	}
}

// Name implements plugin.Input.
func (i *Input) Name() string { return "snmp_poller" }

// Start implements plugin.Input. It loads configuration, builds the internal
// pipeline stages, launches goroutines, and begins sending Envelopes on out.
//
// Start returns quickly — all long-running work is in background goroutines.
func (i *Input) Start(ctx context.Context, out chan<- plugin.Envelope) error {
	// ── 1. Load configuration ───────────────────────────────────────────
	i.logger.Info("snmp_poller: loading configuration")
	loadedCfg, err := config.Load(i.cfg.ConfigPaths, i.logger)
	if err != nil {
		return fmt.Errorf("snmp_poller: load config: %w", err)
	}
	i.loadedCfg = loadedCfg
	i.logger.Info("snmp_poller: configuration loaded",
		"devices", len(loadedCfg.Devices),
		"object_defs", len(loadedCfg.ObjectDefs),
	)

	// ── 2. Create internal channels ─────────────────────────────────────
	i.rawCh = make(chan decoder.RawPollResult, i.cfg.BufferSize)
	i.decodedCh = make(chan decoder.DecodedPollResult, i.cfg.BufferSize)

	// ── 3. Build pipeline components ────────────────────────────────────
	i.prod = metrics.New(metrics.Config{
		CollectorID:         i.cfg.CollectorID,
		EnumEnabled:         i.cfg.EnumEnabled,
		Enums:               loadedCfg.Enums,
		CounterDeltaEnabled: i.cfg.CounterDeltaEnabled,
	}, i.logger)

	i.dec = decoder.NewSNMPDecoder(i.logger)

	i.connPool = poller.NewConnectionPool(i.cfg.PoolOptions, i.logger)
	i.snmpPoller = poller.NewSNMPPoller(i.connPool, i.logger)
	i.workerPool = poller.NewWorkerPool(i.cfg.PollerWorkers, i.snmpPoller, i.rawCh, i.logger)

	i.sched = scheduler.New(loadedCfg, i.workerPool, i.logger)

	// ── 4. Cancellable context for internal goroutines ──────────────────
	pipeCtx, cancel := context.WithCancel(ctx)
	i.cancel = cancel

	// ── 5. Start internal pipeline (from tail to head) ──────────────────
	i.startProduceStage(pipeCtx, out)
	i.startDecodeStage(pipeCtx)

	// Worker pool feeds rawCh.
	i.workerPool.Start(pipeCtx)

	// Scheduler dispatches to worker pool (blocks in its own goroutine).
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.sched.Start(pipeCtx)
	}()

	i.logger.Info("snmp_poller: running",
		"poller_workers", i.cfg.PollerWorkers,
		"buffer_size", i.cfg.BufferSize,
		"entries", i.sched.Entries(),
	)
	return nil
}

// Stop implements plugin.Input. It performs a graceful shutdown:
//
//  1. Cancel internal context (stops scheduler + worker pool sources).
//  2. Wait for scheduler goroutine to exit.
//  3. Drain the worker pool (waits for in-flight polls).
//  4. Close rawCh → decoder drains → closes decodedCh → producer drains.
//  5. Release connection pool.
//
// After Stop returns, no further Envelopes will be sent.
func (i *Input) Stop() {
	i.logger.Info("snmp_poller: shutting down")

	// 1. Signal all goroutines.
	if i.cancel != nil {
		i.cancel()
	}

	// 2. Wait for the scheduler to return.
	if i.sched != nil {
		i.sched.Stop()
	}

	// 3. Drain worker pool.
	if i.workerPool != nil {
		i.workerPool.Stop()
	}

	// 4. Close rawCh → cascade through decode → produce stages.
	if i.rawCh != nil {
		close(i.rawCh)
	}

	// 5. Wait for internal pipeline goroutines.
	i.wg.Wait()

	// 6. Release resources.
	if i.connPool != nil {
		i.connPool.Close()
	}

	i.logger.Info("snmp_poller: shutdown complete")
}

// Reload atomically replaces the running configuration.
func (i *Input) Reload() error {
	i.logger.Info("snmp_poller: reloading configuration")
	newCfg, err := config.Load(i.cfg.ConfigPaths, i.logger)
	if err != nil {
		return fmt.Errorf("snmp_poller: reload config: %w", err)
	}
	i.sched.Reload(newCfg)
	i.loadedCfg = newCfg
	i.logger.Info("snmp_poller: configuration reloaded",
		"devices", len(newCfg.Devices),
		"object_defs", len(newCfg.ObjectDefs),
	)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal pipeline stages
// ─────────────────────────────────────────────────────────────────────────────

// startDecodeStage reads from rawCh, decodes, and forwards to decodedCh.
// Closes decodedCh when rawCh is drained.
func (i *Input) startDecodeStage(_ context.Context) {
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		defer close(i.decodedCh)

		for raw := range i.rawCh {
			decoded, err := i.dec.Decode(raw)
			if err != nil {
				i.logger.Warn("snmp_poller: decode error",
					"device", raw.Device.Hostname,
					"object", raw.ObjectDef.Key,
					"error", err.Error(),
				)
				continue
			}
			if len(decoded.Varbinds) == 0 {
				continue
			}
			i.decodedCh <- decoded
		}
	}()
}

// startProduceStage reads from decodedCh, produces SNMPMetric values, and
// wraps each in an Envelope sent to the plugin output channel.
func (i *Input) startProduceStage(ctx context.Context, out chan<- plugin.Envelope) {
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()

		for decoded := range i.decodedCh {
			metric, err := i.prod.Produce(decoded)
			if err != nil {
				i.logger.Warn("snmp_poller: produce error",
					"device", decoded.Device.Hostname,
					"object", decoded.ObjectDefKey,
					"error", err.Error(),
				)
				continue
			}
			if len(metric.Metrics) == 0 {
				continue
			}

			env := plugin.Envelope{
				Source:    i.Name(),
				Timestamp: metric.Timestamp,
				Metric:    &metric,
			}

			select {
			case out <- env:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// LoadedConfig returns the currently loaded configuration. Useful for
// introspection and testing.
func (i *Input) LoadedConfig() *config.LoadedConfig {
	return i.loadedCfg
}

// ─────────────────────────────────────────────────────────────────────────────
// noopWriter — discard log output when no logger is provided
// ─────────────────────────────────────────────────────────────────────────────

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
