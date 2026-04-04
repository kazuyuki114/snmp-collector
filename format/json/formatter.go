// Package json implements the JSON output formatter for the SNMP Collector
// pipeline. It is the primary (and currently only) serialisation format.
//
// Pipeline position:
//
//	producer/metrics [Stage 4] → format/json [Stage 5] → transport/kafka [Stage 6]
//
// The formatter converts a models.SNMPMetric into a JSON byte slice whose
// schema matches the architecture spec exactly. All json struct tags are
// already declared on the model types themselves, so serialisation is a
// single json.Marshal call with optional indentation.
package json

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"snmp/snmp-collector/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// Formatter interface
// ─────────────────────────────────────────────────────────────────────────────

// Formatter serialises a models.SNMPMetric into a byte slice.
// It is intentionally identical to the interface defined in the architecture
// doc so that alternative formatters (protobuf, Prometheus, InfluxDB …) can
// be added by implementing this interface without touching any other package.
type Formatter interface {
	Format(metric *models.SNMPMetric) ([]byte, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────────────

// Config controls JSONFormatter behaviour.
type Config struct {
	// PrettyPrint emits indented, human-readable JSON when true.
	// Use false (default) in production to minimise byte count on the wire.
	PrettyPrint bool

	// Indent is the indent string used when PrettyPrint=true.
	// Defaults to two spaces when empty and PrettyPrint=true.
	Indent string
}

// ─────────────────────────────────────────────────────────────────────────────
// JSONFormatter
// ─────────────────────────────────────────────────────────────────────────────

// JSONFormatter implements Formatter using encoding/json from the standard
// library. It is safe for concurrent use by multiple goroutines; all fields
// are immutable after construction.
type JSONFormatter struct {
	cfg    Config
	logger *slog.Logger
}

// New constructs a JSONFormatter. If logger is nil, a no-op logger is
// substituted so the formatter never panics on a nil receiver.
func New(cfg Config, logger *slog.Logger) *JSONFormatter {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	if cfg.PrettyPrint && cfg.Indent == "" {
		cfg.Indent = "  "
	}
	return &JSONFormatter{cfg: cfg, logger: logger}
}

// Format serialises metric to JSON. It returns a non-nil error only when
// json.Marshal itself fails (e.g. an un-serialisable value type entered the
// pipeline upstream). The returned byte slice is always non-nil on success.
//
// The JSON schema matches the architecture specification:
//
//	{
//	  "timestamp": "2026-02-26T10:30:00.123Z",
//	  "device": { … },
//	  "metrics": [ { "oid": …, "name": …, "value": …, … } ],
//	  "metadata": { "collector_id": …, "poll_duration_ms": …, "poll_status": … }
//	}
func (f *JSONFormatter) Format(metric *models.SNMPMetric) ([]byte, error) {
	if metric == nil {
		return nil, fmt.Errorf("format/json: metric must not be nil")
	}

	var (
		data []byte
		err  error
	)

	if f.cfg.PrettyPrint {
		data, err = json.MarshalIndent(metric, "", f.cfg.Indent)
	} else {
		data, err = json.Marshal(metric)
	}

	if err != nil {
		f.logger.Error("format/json: marshal failed",
			"collector_id", metric.Metadata.CollectorID,
			"object_def", metric.Device.Hostname,
			"error", err.Error(),
		)
		return nil, fmt.Errorf("format/json: marshal: %w", err)
	}

	f.logger.Debug("format/json: formatted metric",
		"collector_id", metric.Metadata.CollectorID,
		"hostname", metric.Device.Hostname,
		"metric_count", len(metric.Metrics),
		"bytes", len(data),
	)

	return data, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// no-op logger writer
// ─────────────────────────────────────────────────────────────────────────────

// noopWriter discards all log output when no logger is provided.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
