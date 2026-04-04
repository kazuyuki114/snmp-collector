// Package engine implements the core broker loop that wires Input plugins to
// Transport plugins through a formatter stage.
//
// Architecture:
//
//	Input₁ ─┐                            ┌─▶ Transport₁
//	Input₂ ─┤──▶ [envelopeCh] ──▶ Fmt ──┤──▶ Transport₂
//	InputN ─┘                            └─▶ TransportN
//
// All Inputs share a single envelopeCh. The engine runs one or more formatter
// workers that read Envelopes, serialise the metric payload, and fan the
// resulting bytes out to every configured Transport.
//
// Lifecycle is managed via Start / Stop; the engine respects context
// cancellation for graceful shutdown.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	jsonformat "snmp/snmp-collector/format/json"
	"snmp/snmp-collector/plugin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────────────

// Config holds engine-level settings.
type Config struct {
	// BufferSize is the capacity of the shared envelope channel.
	// Default: 10 000.
	BufferSize int

	// FormatWorkers is the number of goroutines running the format → fan-out
	// stage. Default: 50.
	FormatWorkers int

	// PrettyPrint enables indented JSON output.
	PrettyPrint bool
}

func (c *Config) withDefaults() {
	if c.BufferSize <= 0 {
		c.BufferSize = 10_000
	}
	if c.FormatWorkers <= 0 {
		c.FormatWorkers = 50
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Engine
// ─────────────────────────────────────────────────────────────────────────────

// Engine is the central broker that ties Inputs and Transports together.
type Engine struct {
	cfg    Config
	logger *slog.Logger

	inputs     []plugin.Input
	transports []plugin.Transport
	formatter  *jsonformat.JSONFormatter

	envelopeCh chan plugin.Envelope

	cancel context.CancelFunc
	wg     sync.WaitGroup // tracks format/fan-out workers + input-close waiter
}

// New constructs an Engine. Inputs and Transports are registered before Start
// via AddInput / AddTransport.
func New(cfg Config, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	cfg.withDefaults()
	return &Engine{
		cfg:    cfg,
		logger: logger,
	}
}

// AddInput registers an Input plugin. Must be called before Start.
func (e *Engine) AddInput(in plugin.Input) {
	e.inputs = append(e.inputs, in)
}

// AddTransport registers a Transport plugin. Must be called before Start.
func (e *Engine) AddTransport(t plugin.Transport) {
	e.transports = append(e.transports, t)
}

// ─────────────────────────────────────────────────────────────────────────────
// Lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// Start initialises the pipeline and launches all goroutines.
//
// Order:
//  1. Create the shared envelope channel.
//  2. Build the JSON formatter.
//  3. Start format/fan-out workers (they read envelopeCh).
//  4. Start a closer goroutine that waits for all Inputs to finish, then
//     closes envelopeCh so workers drain cleanly.
//  5. Start each Input plugin (they write to envelopeCh).
func (e *Engine) Start(ctx context.Context) error {
	if len(e.inputs) == 0 {
		return fmt.Errorf("engine: no inputs registered")
	}
	if len(e.transports) == 0 {
		return fmt.Errorf("engine: no transports registered")
	}

	e.envelopeCh = make(chan plugin.Envelope, e.cfg.BufferSize)

	e.formatter = jsonformat.New(jsonformat.Config{
		PrettyPrint: e.cfg.PrettyPrint,
	}, e.logger)

	pipeCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	// ── Format / fan-out workers ────────────────────────────────────────
	for i := 0; i < e.cfg.FormatWorkers; i++ {
		e.wg.Add(1)
		go e.formatWorker(pipeCtx, i)
	}

	// ── Input closer: waits for all inputs to stop, then closes
	//    envelopeCh so format workers drain. ──────────────────────────────
	var inputWg sync.WaitGroup
	for _, in := range e.inputs {
		inputWg.Add(1)
		in := in // capture
		go func() {
			defer inputWg.Done()
			<-pipeCtx.Done()
			in.Stop()
		}()
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		inputWg.Wait()
		close(e.envelopeCh)
		e.logger.Info("engine: envelope channel closed — all inputs stopped")
	}()

	// ── Start inputs ────────────────────────────────────────────────────
	for _, in := range e.inputs {
		e.logger.Info("engine: starting input", "name", in.Name())
		if err := in.Start(pipeCtx, e.envelopeCh); err != nil {
			// Non-fatal: log and continue with remaining inputs.
			e.logger.Error("engine: input failed to start",
				"name", in.Name(),
				"error", err.Error(),
			)
		}
	}

	e.logger.Info("engine: running",
		"inputs", len(e.inputs),
		"transports", len(e.transports),
		"format_workers", e.cfg.FormatWorkers,
		"buffer_size", e.cfg.BufferSize,
	)
	return nil
}

// Stop performs a graceful shutdown:
//
//  1. Cancel the pipeline context — Inputs begin draining.
//  2. Wait for all format workers to finish (they exit when envelopeCh is
//     closed after all Inputs have stopped).
//  3. Close all Transports.
func (e *Engine) Stop() {
	e.logger.Info("engine: shutting down")

	// 1. Signal all goroutines.
	if e.cancel != nil {
		e.cancel()
	}

	// 2. Wait for format workers + input-closer goroutine.
	e.wg.Wait()

	// 3. Close transports.
	for _, t := range e.transports {
		if err := t.Close(); err != nil {
			e.logger.Error("engine: transport close error",
				"name", t.Name(),
				"error", err.Error(),
			)
		}
	}

	e.logger.Info("engine: shutdown complete")
}

// ─────────────────────────────────────────────────────────────────────────────
// Format / fan-out worker
// ─────────────────────────────────────────────────────────────────────────────

// formatWorker reads Envelopes from the shared channel, formats the metric
// payload to JSON, and sends the result to every registered Transport.
func (e *Engine) formatWorker(ctx context.Context, id int) {
	defer e.wg.Done()

	for env := range e.envelopeCh {
		if !env.Valid() {
			e.logger.Debug("engine: dropping invalid envelope", "source", env.Source)
			continue
		}

		data, err := e.formatter.Format(env.Metric)
		if err != nil {
			e.logger.Warn("engine: format error",
				"source", env.Source,
				"device", env.Metric.Device.Hostname,
				"error", err.Error(),
			)
			continue
		}

		// Fan-out to all transports.
		for _, t := range e.transports {
			if sendErr := t.Send(data); sendErr != nil {
				e.logger.Error("engine: transport send error",
					"transport", t.Name(),
					"source", env.Source,
					"device", env.Metric.Device.Hostname,
					"error", sendErr.Error(),
					"bytes", len(data),
				)
			}
		}
	}

	e.logger.Debug("engine: format worker exited", "worker_id", id)
}
