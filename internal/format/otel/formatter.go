// Package otel implements an OpenTelemetry (OTLP) JSON formatter for the
// SNMP Collector pipeline. It converts a models.SNMPMetric into an
// ExportMetricsServiceRequest as specified by the OTLP/JSON protocol:
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
//
// Pipeline position:
//
//	producer/metrics [Stage 4] → format/otel [Stage 5] → transport [Stage 6]
//
// No external OpenTelemetry SDK is required; the OTLP JSON schema is
// implemented directly using standard library types.
package otel

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"snmp/snmp-collector/internal/models"
	"snmp/snmp-collector/internal/noop"
)

// OTLP aggregation temporality constants (proto enum values).
const (
	temporalityCumulative = 2 // AGGREGATION_TEMPORALITY_CUMULATIVE
)

// ─────────────────────────────────────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────────────────────────────────────

// Config holds constructor options for the OTel formatter.
type Config struct {
	// ScopeName is the OpenTelemetry instrumentation scope name written into
	// every ExportMetricsServiceRequest. Defaults to "snmp-collector".
	ScopeName string `yaml:"scope_name"`

	// ScopeVersion is the instrumentation scope version string.
	ScopeVersion string `yaml:"scope_version"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Formatter
// ─────────────────────────────────────────────────────────────────────────────

// Formatter converts a models.SNMPMetric to OTLP JSON bytes.
// It implements the same Format interface as format/json.Formatter.
type Formatter struct {
	cfg    Config
	logger *slog.Logger
}

// New constructs an OTel Formatter. Pass nil for logger to use a no-op logger.
func New(cfg Config, logger *slog.Logger) *Formatter {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noop.Writer{}, nil))
	}
	if cfg.ScopeName == "" {
		cfg.ScopeName = "snmp-collector"
	}
	return &Formatter{cfg: cfg, logger: logger}
}

// Format converts metric to an OTLP ExportMetricsServiceRequest JSON payload.
//
// Mapping summary:
//   - models.Device     → resource.attributes
//   - models.Metric     → scopeMetrics[0].metrics (grouped by Name)
//   - Counter32/64      → Sum  (isMonotonic=true, temporality=CUMULATIVE)
//   - All other numeric → Gauge
//   - String/[]byte     → skipped (not representable as an OTLP number)
//   - Metric.Tags +
//     Metric.Instance   → dataPoint.attributes
func (f *Formatter) Format(metric *models.SNMPMetric) ([]byte, error) {
	if metric == nil {
		return nil, fmt.Errorf("format/otel: metric must not be nil")
	}

	req := f.convert(metric)

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("format/otel: marshal: %w", err)
	}

	f.logger.Debug("format/otel: formatted metric",
		"hostname", metric.Device.Hostname,
		"metric_count", len(metric.Metrics),
		"bytes", len(data),
	)

	return data, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Conversion
// ─────────────────────────────────────────────────────────────────────────────

func (f *Formatter) convert(m *models.SNMPMetric) exportMetricsServiceRequest {
	timeNano := strconv.FormatInt(m.Timestamp.UnixNano(), 10)

	// Build resource attributes from the device + collector identity.
	res := resource{Attributes: deviceAttributes(m.Device, m.Metadata.CollectorID)}

	// Group models.Metric values by name so each unique metric name becomes
	// one OTLP Metric with multiple data points (one per instance / tag set).
	type metricGroup struct {
		unit      string
		isCounter bool
		points    []numberDataPoint
	}
	// Use an ordered slice + index map to preserve deterministic output order.
	order := make([]string, 0, len(m.Metrics))
	groups := make(map[string]*metricGroup, len(m.Metrics))

	for _, metric := range m.Metrics {
		dp, ok := toDataPoint(metric, timeNano)
		if !ok {
			// String/byte values cannot be represented as OTLP numbers — skip.
			continue
		}

		g, exists := groups[metric.Name]
		if !exists {
			g = &metricGroup{
				unit:      syntaxToUnit(metric.Syntax),
				isCounter: isCounter(metric.Type),
			}
			groups[metric.Name] = g
			order = append(order, metric.Name)
		}
		g.points = append(g.points, dp)
	}

	// Build the OTLP metrics slice.
	otelMetrics := make([]otelMetric, 0, len(order))
	for _, name := range order {
		g := groups[name]
		om := otelMetric{
			Name: name,
			Unit: g.unit,
		}
		if g.isCounter {
			om.Sum = &sum{
				DataPoints:             g.points,
				AggregationTemporality: temporalityCumulative,
				IsMonotonic:            true,
			}
		} else {
			om.Gauge = &gauge{DataPoints: g.points}
		}
		otelMetrics = append(otelMetrics, om)
	}

	return exportMetricsServiceRequest{
		ResourceMetrics: []resourceMetrics{
			{
				Resource: res,
				ScopeMetrics: []scopeMetrics{
					{
						Scope: instrumentationScope{
							Name:       f.cfg.ScopeName,
							Version:    f.cfg.ScopeVersion,
							Attributes: scopeAttributes(m.Metadata),
						},
						Metrics: otelMetrics,
					},
				},
			},
		},
	}
}

// scopeAttributes converts MetricMetadata fields into OTLP scope attributes.
// These carry collector-side operational context that has no place in the
// device resource or individual data-point attributes.
func scopeAttributes(md models.MetricMetadata) []keyValue {
	attrs := []keyValue{
		strAttr("poll.status", md.PollStatus),
		strAttr("poll.duration_ms", strconv.FormatInt(md.PollDurationMs, 10)),
	}
	if md.ObjectKey != "" {
		attrs = append(attrs, strAttr("poll.object_key", md.ObjectKey))
	}
	if md.ErrorType != "" {
		attrs = append(attrs, strAttr("poll.error_type", md.ErrorType))
	}
	if md.ErrorDetail != "" {
		attrs = append(attrs, strAttr("poll.error_detail", md.ErrorDetail))
	}
	return attrs
}

// toDataPoint converts a single models.Metric to a numberDataPoint.
// Returns false when the metric value cannot be represented as an OTLP number
// (e.g. string, []byte).
func toDataPoint(m models.Metric, timeNano string) (numberDataPoint, bool) {
	dp := numberDataPoint{
		Attributes:   metricAttributes(m),
		TimeUnixNano: timeNano,
	}

	switch v := m.Value.(type) {
	case int64:
		s := strconv.FormatInt(v, 10)
		dp.AsInt = &s
	case uint64:
		s := strconv.FormatUint(v, 10)
		dp.AsInt = &s
	case float64:
		dp.AsDouble = &v
	default:
		return numberDataPoint{}, false
	}

	return dp, true
}

// deviceAttributes converts a models.Device and the collector identity into
// OTLP resource attributes. collectorID is written as "collector.id" so
// consumers can identify which collector instance produced the data — critical
// in Active/Standby HA deployments where two nodes observe the same devices.
func deviceAttributes(d models.Device, collectorID string) []keyValue {
	attrs := []keyValue{
		strAttr("host.name", d.Hostname),
		strAttr("net.host.ip", d.IPAddress),
		strAttr("snmp.version", d.SNMPVersion),
	}
	if collectorID != "" {
		attrs = append(attrs, strAttr("collector.id", collectorID))
	}
	if d.Vendor != "" {
		attrs = append(attrs, strAttr("device.vendor", d.Vendor))
	}
	if d.Model != "" {
		attrs = append(attrs, strAttr("device.model", d.Model))
	}
	if d.SysDescr != "" {
		attrs = append(attrs, strAttr("device.sys_descr", d.SysDescr))
	}
	if d.SysLocation != "" {
		attrs = append(attrs, strAttr("device.sys_location", d.SysLocation))
	}
	if d.SysContact != "" {
		attrs = append(attrs, strAttr("device.sys_contact", d.SysContact))
	}
	for k, v := range d.Tags {
		attrs = append(attrs, strAttr("device.tag."+k, v))
	}
	return attrs
}

// metricAttributes builds the OTLP dataPoint attributes from Metric tags and instance.
func metricAttributes(m models.Metric) []keyValue {
	attrs := make([]keyValue, 0, len(m.Tags)+1)
	if m.Instance != "" {
		attrs = append(attrs, strAttr("instance", m.Instance))
	}
	for k, v := range m.Tags {
		attrs = append(attrs, strAttr(k, v))
	}
	return attrs
}

// ─────────────────────────────────────────────────────────────────────────────
// SNMP type helpers
// ─────────────────────────────────────────────────────────────────────────────

// isCounter returns true for SNMP types that map to an OTLP Sum instrument.
func isCounter(snmpType string) bool {
	switch snmpType {
	case "Counter32", "Counter64":
		return true
	default:
		return false
	}
}

// syntaxToUnit maps SNMP config syntax strings to OTLP unit strings.
// See: https://ucum.org/ucum (OTLP uses UCUM unit strings).
func syntaxToUnit(syntax string) string {
	switch syntax {
	case "BandwidthBits":
		return "bit/s"
	case "BandwidthKBits":
		return "kbit/s"
	case "BandwidthMBits":
		return "Mbit/s"
	case "BandwidthGBits":
		return "Gbit/s"
	case "TimeTicks":
		return "cs" // centiseconds (SNMP TimeTicks unit)
	default:
		return ""
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// OTLP JSON schema types
// All int64/uint64 fields use string encoding per the proto3 JSON mapping spec.
// ─────────────────────────────────────────────────────────────────────────────

type exportMetricsServiceRequest struct {
	ResourceMetrics []resourceMetrics `json:"resourceMetrics"`
}

type resourceMetrics struct {
	Resource     resource       `json:"resource"`
	ScopeMetrics []scopeMetrics `json:"scopeMetrics"`
}

type resource struct {
	Attributes []keyValue `json:"attributes,omitempty"`
}

type scopeMetrics struct {
	Scope   instrumentationScope `json:"scope"`
	Metrics []otelMetric         `json:"metrics"`
}

type instrumentationScope struct {
	Name       string     `json:"name"`
	Version    string     `json:"version,omitempty"`
	Attributes []keyValue `json:"attributes,omitempty"`
}

type otelMetric struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Gauge       *gauge `json:"gauge,omitempty"`
	Sum         *sum   `json:"sum,omitempty"`
}

type gauge struct {
	DataPoints []numberDataPoint `json:"dataPoints"`
}

type sum struct {
	DataPoints             []numberDataPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

// numberDataPoint holds either asInt (proto int64 as decimal string) or
// asDouble (proto double as JSON number), never both.
type numberDataPoint struct {
	Attributes   []keyValue `json:"attributes,omitempty"`
	TimeUnixNano string     `json:"timeUnixNano"`
	AsInt        *string    `json:"asInt,omitempty"`    // int64/uint64 encoded as decimal string
	AsDouble     *float64   `json:"asDouble,omitempty"` // float64 as JSON number
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type anyValue struct {
	StringValue *string `json:"stringValue,omitempty"`
}

// strAttr constructs a string-valued OTLP KeyValue.
func strAttr(key, value string) keyValue {
	return keyValue{Key: key, Value: anyValue{StringValue: &value}}
}
