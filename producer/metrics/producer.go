package metrics

import (
	"log/slog"

	"snmp/snmp-collector/models"
	"snmp/snmp-collector/snmp/decoder"
)

// ─────────────────────────────────────────────────────────────────────────────
// Producer interface
// ─────────────────────────────────────────────────────────────────────────────

// Producer converts a decoder.DecodedPollResult into a models.SNMPMetric ready
// for the formatter stage. Implementations must be safe for concurrent use —
// the pipeline runs 100 producer goroutines by default, all sharing one instance.
type Producer interface {
	Produce(decoded decoder.DecodedPollResult) (models.SNMPMetric, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────────────────────────────────────

// Config holds constructor options for MetricsProducer.
type Config struct {
	// CollectorID is a stable identifier for this collector instance, written
	// into every MetricMetadata struct. Typically the hostname or pod name.
	CollectorID string

	// EnumEnabled mirrors -PROCESSOR_SNMP_ENUM_ENABLE. When true, EnumInteger,
	// EnumBitmap, and EnumObjectIdentifier values are resolved to text labels
	// using the provided Enums registry.
	EnumEnabled bool

	// Enums is the pre-loaded enum registry. Required when EnumEnabled=true.
	// If nil and EnumEnabled=true, enum resolution is silently skipped.
	Enums *EnumRegistry

	// CounterDeltaEnabled controls whether Counter32/Counter64 values are
	// replaced by their per-interval deltas. When false, raw cumulative values
	// are forwarded as-is (useful for downstream systems that prefer to compute
	// their own rates).
	CounterDeltaEnabled bool
}

// ─────────────────────────────────────────────────────────────────────────────
// MetricsProducer — production implementation
// ─────────────────────────────────────────────────────────────────────────────

// MetricsProducer is the production Producer implementation.
// It is stateless w.r.t. the pipeline messages; mutable state is confined
// to CounterState (protected by its own mutex) and EnumRegistry (read-only
// after construction).
type MetricsProducer struct {
	cfg      Config
	counters *CounterState
	logger   *slog.Logger
}

// New constructs a MetricsProducer. The logger should be a JSON-format slog
// instance matching -log.fmt=json. Pass nil for a no-op logger.
func New(cfg Config, logger *slog.Logger) *MetricsProducer {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopProducerWriter{}, nil))
	}

	var cs *CounterState
	if cfg.CounterDeltaEnabled {
		cs = NewCounterState()
	}

	return &MetricsProducer{
		cfg:      cfg,
		counters: cs,
		logger:   logger,
	}
}

// Produce implements Producer.
//
// It calls Build with the configured options and logs a debug line on success.
// The error return is reserved for future validation; Build itself is currently
// infallible — all conversion problems are handled by the decoder upstream.
func (p *MetricsProducer) Produce(decoded decoder.DecodedPollResult) (models.SNMPMetric, error) {
	var enums *EnumRegistry
	if p.cfg.EnumEnabled {
		enums = p.cfg.Enums
	}

	opts := BuildOptions{
		CollectorID: p.cfg.CollectorID,
		PollStatus:  "success",
		Enums:       enums,
		Counters:    p.counters,
	}

	result := Build(decoded, opts)

	p.logger.Debug("produce: assembled SNMPMetric",
		"device", decoded.Device.Hostname,
		"object", decoded.ObjectDefKey,
		"metric_count", len(result.Metrics),
		"poll_duration_ms", decoded.PollDurationMs,
	)

	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// noopProducerWriter — discards log output when no logger is provided
// ─────────────────────────────────────────────────────────────────────────────

type noopProducerWriter struct{}

func (noopProducerWriter) Write(p []byte) (int, error) { return len(p), nil }
