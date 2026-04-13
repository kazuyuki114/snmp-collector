package otel

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"snmp/snmp-collector/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func newFormatter() *Formatter {
	return New(Config{ScopeName: "snmp-collector", ScopeVersion: "1.0.0"}, nil)
}

func mustFormat(t *testing.T, f *Formatter, m *models.SNMPMetric) exportMetricsServiceRequest {
	t.Helper()
	data, err := f.Format(m)
	if err != nil {
		t.Fatalf("Format error: %v", err)
	}
	var req exportMetricsServiceRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return req
}

func baseMetric() *models.SNMPMetric {
	ts := time.Date(2026, 4, 13, 10, 0, 0, 0, time.UTC)
	return &models.SNMPMetric{
		Timestamp: ts,
		Device: models.Device{
			Hostname:    "sw1",
			IPAddress:   "192.168.1.1",
			SNMPVersion: "2c",
		},
		Metadata: models.MetricMetadata{
			CollectorID: "col-01",
			PollStatus:  "success",
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_NilMetric(t *testing.T) {
	f := newFormatter()
	_, err := f.Format(nil)
	if err == nil {
		t.Fatal("expected error for nil metric")
	}
}

func TestFormat_ResourceAttributes(t *testing.T) {
	m := baseMetric()
	m.Device.Vendor = "Cisco"
	m.Device.SysLocation = "DC1"

	req := mustFormat(t, newFormatter(), m)

	if len(req.ResourceMetrics) != 1 {
		t.Fatalf("expected 1 resourceMetrics, got %d", len(req.ResourceMetrics))
	}
	attrs := req.ResourceMetrics[0].Resource.Attributes
	find := func(key string) string {
		for _, a := range attrs {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue
			}
		}
		return ""
	}

	if got := find("host.name"); got != "sw1" {
		t.Errorf("host.name = %q, want %q", got, "sw1")
	}
	if got := find("net.host.ip"); got != "192.168.1.1" {
		t.Errorf("net.host.ip = %q, want %q", got, "192.168.1.1")
	}
	if got := find("snmp.version"); got != "2c" {
		t.Errorf("snmp.version = %q, want %q", got, "2c")
	}
	if got := find("device.vendor"); got != "Cisco" {
		t.Errorf("device.vendor = %q, want %q", got, "Cisco")
	}
	if got := find("device.sys_location"); got != "DC1" {
		t.Errorf("device.sys_location = %q, want %q", got, "DC1")
	}
}

func TestFormat_ScopeNameVersion(t *testing.T) {
	f := New(Config{ScopeName: "myapp", ScopeVersion: "2.0"}, nil)
	req := mustFormat(t, f, baseMetric())

	scope := req.ResourceMetrics[0].ScopeMetrics[0].Scope
	if scope.Name != "myapp" {
		t.Errorf("scope.name = %q, want %q", scope.Name, "myapp")
	}
	if scope.Version != "2.0" {
		t.Errorf("scope.version = %q, want %q", scope.Version, "2.0")
	}
}

func TestFormat_DefaultScopeName(t *testing.T) {
	f := New(Config{}, nil)
	req := mustFormat(t, f, baseMetric())
	scope := req.ResourceMetrics[0].ScopeMetrics[0].Scope
	if scope.Name != "snmp-collector" {
		t.Errorf("default scope.name = %q, want %q", scope.Name, "snmp-collector")
	}
}

func TestFormat_Counter64_BecomesSum(t *testing.T) {
	m := baseMetric()
	val := uint64(12345)
	_ = val
	m.Metrics = []models.Metric{
		{
			Name:     "netif.bytes.in",
			OID:      "1.3.6.1.2.1.2.2.1.10.1",
			Instance: "1",
			Value:    uint64(12345),
			Type:     "Counter64",
			Syntax:   "Counter64",
			Tags:     map[string]string{"ifDescr": "Gi0/1"},
		},
	}

	req := mustFormat(t, newFormatter(), m)
	metrics := req.ResourceMetrics[0].ScopeMetrics[0].Metrics

	if len(metrics) != 1 {
		t.Fatalf("expected 1 metric, got %d", len(metrics))
	}
	om := metrics[0]
	if om.Name != "netif.bytes.in" {
		t.Errorf("name = %q, want %q", om.Name, "netif.bytes.in")
	}
	if om.Sum == nil {
		t.Fatal("Counter64 should produce a Sum metric, got nil")
	}
	if om.Gauge != nil {
		t.Error("Counter64 should not produce a Gauge metric")
	}
	if !om.Sum.IsMonotonic {
		t.Error("Sum.IsMonotonic should be true for Counter64")
	}
	if om.Sum.AggregationTemporality != temporalityCumulative {
		t.Errorf("AggregationTemporality = %d, want %d",
			om.Sum.AggregationTemporality, temporalityCumulative)
	}

	if len(om.Sum.DataPoints) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(om.Sum.DataPoints))
	}
	dp := om.Sum.DataPoints[0]
	if dp.AsInt == nil || *dp.AsInt != "12345" {
		t.Errorf("asInt = %v, want %q", dp.AsInt, "12345")
	}
}

func TestFormat_Counter32_BecomesSum(t *testing.T) {
	m := baseMetric()
	m.Metrics = []models.Metric{
		{Name: "snmp.counter", Value: int64(999), Type: "Counter32", Syntax: "Counter32"},
	}
	req := mustFormat(t, newFormatter(), m)
	om := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0]
	if om.Sum == nil {
		t.Fatal("Counter32 should produce a Sum metric")
	}
}

func TestFormat_Gauge32_BecomesGauge(t *testing.T) {
	m := baseMetric()
	m.Metrics = []models.Metric{
		{Name: "cpu.util", Value: int64(42), Type: "Gauge32", Syntax: "Gauge32"},
	}
	req := mustFormat(t, newFormatter(), m)
	om := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0]
	if om.Gauge == nil {
		t.Fatal("Gauge32 should produce a Gauge metric")
	}
	if om.Sum != nil {
		t.Error("Gauge32 should not produce a Sum metric")
	}
}

func TestFormat_Int64Value(t *testing.T) {
	m := baseMetric()
	m.Metrics = []models.Metric{
		{Name: "sys.uptime", Value: int64(-1), Type: "Integer", Syntax: "Integer"},
	}
	req := mustFormat(t, newFormatter(), m)
	dp := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Gauge.DataPoints[0]
	if dp.AsInt == nil || *dp.AsInt != "-1" {
		t.Errorf("asInt = %v, want %q", dp.AsInt, "-1")
	}
	if dp.AsDouble != nil {
		t.Error("int64 should not set asDouble")
	}
}

func TestFormat_Float64Value(t *testing.T) {
	m := baseMetric()
	fval := 1.5
	m.Metrics = []models.Metric{
		{Name: "bw.util", Value: float64(1.5), Type: "Gauge32", Syntax: "BandwidthMBits"},
	}
	req := mustFormat(t, newFormatter(), m)
	dp := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Gauge.DataPoints[0]
	if dp.AsDouble == nil || *dp.AsDouble != fval {
		t.Errorf("asDouble = %v, want %v", dp.AsDouble, fval)
	}
	if dp.AsInt != nil {
		t.Error("float64 should not set asInt")
	}
}

func TestFormat_StringValue_Skipped(t *testing.T) {
	m := baseMetric()
	m.Metrics = []models.Metric{
		{Name: "sys.descr", Value: "some text", Type: "OctetString", Syntax: "DisplayString"},
	}
	req := mustFormat(t, newFormatter(), m)
	metrics := req.ResourceMetrics[0].ScopeMetrics[0].Metrics
	if len(metrics) != 0 {
		t.Errorf("string-valued metric should be skipped, got %d metrics", len(metrics))
	}
}

func TestFormat_MultipleInstances_SingleOTelMetric(t *testing.T) {
	// Two metrics with the same name but different instances → one OTelMetric,
	// two data points.
	m := baseMetric()
	m.Metrics = []models.Metric{
		{Name: "netif.bytes.in", Value: uint64(100), Type: "Counter64", Instance: "1"},
		{Name: "netif.bytes.in", Value: uint64(200), Type: "Counter64", Instance: "2"},
	}
	req := mustFormat(t, newFormatter(), m)
	metrics := req.ResourceMetrics[0].ScopeMetrics[0].Metrics

	if len(metrics) != 1 {
		t.Fatalf("expected 1 OTelMetric for same name, got %d", len(metrics))
	}
	if len(metrics[0].Sum.DataPoints) != 2 {
		t.Errorf("expected 2 data points, got %d", len(metrics[0].Sum.DataPoints))
	}
}

func TestFormat_MultipleNames_MultipleOTelMetrics(t *testing.T) {
	m := baseMetric()
	m.Metrics = []models.Metric{
		{Name: "netif.bytes.in", Value: uint64(100), Type: "Counter64"},
		{Name: "netif.bytes.out", Value: uint64(50), Type: "Counter64"},
		{Name: "cpu.util", Value: int64(30), Type: "Gauge32"},
	}
	req := mustFormat(t, newFormatter(), m)
	metrics := req.ResourceMetrics[0].ScopeMetrics[0].Metrics

	if len(metrics) != 3 {
		t.Errorf("expected 3 OTelMetrics, got %d", len(metrics))
	}
}

func TestFormat_DataPointAttributes_InstanceAndTags(t *testing.T) {
	m := baseMetric()
	m.Metrics = []models.Metric{
		{
			Name:     "netif.bytes.in",
			Value:    uint64(1),
			Type:     "Counter64",
			Instance: "3",
			Tags:     map[string]string{"ifDescr": "eth0"},
		},
	}
	req := mustFormat(t, newFormatter(), m)
	dp := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Sum.DataPoints[0]

	find := func(key string) string {
		for _, a := range dp.Attributes {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue
			}
		}
		return ""
	}

	if got := find("instance"); got != "3" {
		t.Errorf("instance attr = %q, want %q", got, "3")
	}
	if got := find("ifDescr"); got != "eth0" {
		t.Errorf("ifDescr attr = %q, want %q", got, "eth0")
	}
}

func TestFormat_TimeUnixNano(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	m := baseMetric()
	m.Timestamp = ts
	m.Metrics = []models.Metric{
		{Name: "x", Value: int64(1), Type: "Gauge32"},
	}
	req := mustFormat(t, newFormatter(), m)
	dp := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Gauge.DataPoints[0]

	want := strconv.FormatInt(ts.UnixNano(), 10)
	if dp.TimeUnixNano != want {
		t.Errorf("timeUnixNano = %q, want %q", dp.TimeUnixNano, want)
	}
}

func TestFormat_UnitMapping(t *testing.T) {
	tests := []struct {
		syntax string
		unit   string
	}{
		{"BandwidthBits", "bit/s"},
		{"BandwidthKBits", "kbit/s"},
		{"BandwidthMBits", "Mbit/s"},
		{"BandwidthGBits", "Gbit/s"},
		{"TimeTicks", "cs"},
		{"Counter64", ""},
		{"Gauge32", ""},
	}
	for _, tt := range tests {
		t.Run(tt.syntax, func(t *testing.T) {
			got := syntaxToUnit(tt.syntax)
			if got != tt.unit {
				t.Errorf("syntaxToUnit(%q) = %q, want %q", tt.syntax, got, tt.unit)
			}
		})
	}
}

func TestFormat_EmptyMetrics_EmptyOTelMetrics(t *testing.T) {
	m := baseMetric()
	// No metrics.
	req := mustFormat(t, newFormatter(), m)
	metrics := req.ResourceMetrics[0].ScopeMetrics[0].Metrics
	if len(metrics) != 0 {
		t.Errorf("expected 0 metrics, got %d", len(metrics))
	}
}

func TestFormat_DeviceTags_InResourceAttributes(t *testing.T) {
	m := baseMetric()
	m.Device.Tags = map[string]string{"region": "us-east-1", "env": "prod"}
	req := mustFormat(t, newFormatter(), m)

	attrs := req.ResourceMetrics[0].Resource.Attributes
	tagMap := map[string]string{}
	for _, a := range attrs {
		if a.Value.StringValue != nil {
			tagMap[a.Key] = *a.Value.StringValue
		}
	}

	if tagMap["device.tag.region"] != "us-east-1" {
		t.Errorf("device.tag.region = %q, want %q", tagMap["device.tag.region"], "us-east-1")
	}
	if tagMap["device.tag.env"] != "prod" {
		t.Errorf("device.tag.env = %q, want %q", tagMap["device.tag.env"], "prod")
	}
}
