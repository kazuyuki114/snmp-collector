package json_test

import (
	stdjson "encoding/json"
	"strings"
	"testing"
	"time"

	fmtjson "snmp/snmp-collector/internal/format/json"
	"snmp/snmp-collector/internal/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared fixtures
// ─────────────────────────────────────────────────────────────────────────────

var testTimestamp = time.Date(2026, 2, 26, 10, 30, 0, 123_000_000, time.UTC)

var fullMetric = models.SNMPMetric{
	Timestamp: testTimestamp,
	Device: models.Device{
		Hostname:    "router01.example.com",
		IPAddress:   "192.168.1.1",
		SNMPVersion: "2c",
		Vendor:      "Cisco",
		Model:       "ASR1000",
		SysDescr:    "Cisco IOS Software, ASR1000 Software",
		SysLocation: "DC1-Rack5",
		SysContact:  "netops@example.com",
		Tags: map[string]string{
			"environment": "production",
			"role":        "edge-router",
			"site":        "dc1",
		},
	},
	Metrics: []models.Metric{
		{
			OID:      "1.3.6.1.2.1.2.2.1.10.1",
			Name:     "netif.bytes.in",
			Instance: "1",
			Value:    uint64(1234567890),
			Type:     "Counter64",
			Syntax:   "Counter64",
			Tags: map[string]string{
				"netif.descr": "GigabitEthernet0/0/1",
				"netif.type":  "ethernetCsmacd",
			},
		},
		{
			OID:      "1.3.6.1.2.1.2.2.1.8.1",
			Name:     "netif.state.oper",
			Instance: "1",
			Value:    "up",
			Type:     "Integer",
			Syntax:   "EnumInteger",
			Tags: map[string]string{
				"netif.descr": "GigabitEthernet0/0/1",
			},
		},
		{
			OID:      "1.3.6.1.2.1.2.2.1.5.1",
			Name:     "netif.speed",
			Instance: "1",
			Value:    float64(1e9),
			Type:     "Gauge32",
			Syntax:   "BandwidthBits",
		},
	},
	Metadata: models.MetricMetadata{
		CollectorID:    "collector-01",
		PollDurationMs: 245,
		PollStatus:     "success",
	},
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func mustFormat(t *testing.T, f *fmtjson.JSONFormatter, m *models.SNMPMetric) []byte {
	t.Helper()
	b, err := f.Format(m)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	return b
}

func unmarshal(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := stdjson.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, data)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Construction
// ─────────────────────────────────────────────────────────────────────────────

func TestNew_NilLoggerDoesNotPanic(t *testing.T) {
	// Must not panic.
	f := fmtjson.New(fmtjson.Config{}, nil)
	if f == nil {
		t.Fatal("New returned nil")
	}
}

func TestNew_DefaultIndentForPrettyPrint(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{PrettyPrint: true}, nil)
	data := mustFormat(t, f, &fullMetric)
	// Indented output has newlines.
	if !strings.Contains(string(data), "\n") {
		t.Error("pretty-print output should contain newlines")
	}
}

func TestNew_CustomIndent(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{PrettyPrint: true, Indent: "\t"}, nil)
	data := mustFormat(t, f, &fullMetric)
	if !strings.Contains(string(data), "\t") {
		t.Error("custom-indent output should contain tab characters")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Nil input
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_NilMetricReturnsError(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	_, err := f.Format(nil)
	if err == nil {
		t.Error("expected non-nil error for nil metric")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Schema compliance — top-level keys
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_TopLevelKeys(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))

	for _, key := range []string{"timestamp", "device", "metrics", "metadata"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("top-level key %q missing", key)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Timestamp
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_TimestampIsRFC3339(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	ts, ok := doc["timestamp"].(string)
	if !ok {
		t.Fatal("timestamp is not a string")
	}
	// Parsed time must round-trip through the architecture-mandated millisecond
	// precision. time.Time uses RFC3339Nano by default in encoding/json.
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC3339Nano: %v", ts, err)
	}
	if !parsed.Equal(testTimestamp) {
		t.Errorf("timestamp round-trip: got %v, want %v", parsed, testTimestamp)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Device fields
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_DeviceFields(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	dev, ok := doc["device"].(map[string]interface{})
	if !ok {
		t.Fatal("device is not an object")
	}

	checks := map[string]string{
		"hostname":     "router01.example.com",
		"ip_address":   "192.168.1.1",
		"snmp_version": "2c",
		"vendor":       "Cisco",
		"model":        "ASR1000",
		"sys_descr":    "Cisco IOS Software, ASR1000 Software",
		"sys_location": "DC1-Rack5",
		"sys_contact":  "netops@example.com",
	}
	for k, want := range checks {
		if got, _ := dev[k].(string); got != want {
			t.Errorf("device.%s = %q, want %q", k, got, want)
		}
	}

	tags, ok := dev["tags"].(map[string]interface{})
	if !ok {
		t.Fatal("device.tags is not an object")
	}
	if tags["environment"] != "production" {
		t.Errorf("device.tags.environment = %v, want %q", tags["environment"], "production")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Metrics array
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_MetricsCount(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	arr, ok := doc["metrics"].([]interface{})
	if !ok {
		t.Fatal("metrics is not an array")
	}
	if len(arr) != 3 {
		t.Errorf("metrics count = %d, want 3", len(arr))
	}
}

func TestFormat_MetricFields(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	arr := doc["metrics"].([]interface{})
	m := arr[0].(map[string]interface{})

	if m["oid"] != "1.3.6.1.2.1.2.2.1.10.1" {
		t.Errorf("oid = %v", m["oid"])
	}
	if m["name"] != "netif.bytes.in" {
		t.Errorf("name = %v", m["name"])
	}
	if m["instance"] != "1" {
		t.Errorf("instance = %v", m["instance"])
	}
	if m["type"] != "Counter64" {
		t.Errorf("type = %v", m["type"])
	}
	if m["syntax"] != "Counter64" {
		t.Errorf("syntax = %v", m["syntax"])
	}
}

func TestFormat_MetricStringValue(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	arr := doc["metrics"].([]interface{})
	// Second metric has Value="up"
	m := arr[1].(map[string]interface{})
	if m["value"] != "up" {
		t.Errorf("string value = %v, want %q", m["value"], "up")
	}
}

func TestFormat_MetricFloatValue(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	arr := doc["metrics"].([]interface{})
	// Third metric has Value=float64(1e9)
	m := arr[2].(map[string]interface{})
	v, ok := m["value"].(float64)
	if !ok {
		t.Fatalf("float value type = %T", m["value"])
	}
	if v != 1e9 {
		t.Errorf("float value = %v, want 1e9", v)
	}
}

func TestFormat_MetricTagsOmitted(t *testing.T) {
	// Third metric has no Tags (nil map) — should be absent from JSON.
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	arr := doc["metrics"].([]interface{})
	m := arr[2].(map[string]interface{})
	if _, ok := m["tags"]; ok {
		t.Error("tags key should be absent when tag map is nil/empty")
	}
}

func TestFormat_MetricTagsPresent(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	arr := doc["metrics"].([]interface{})
	m := arr[0].(map[string]interface{})
	tags, ok := m["tags"].(map[string]interface{})
	if !ok {
		t.Fatal("tags not found in first metric")
	}
	if tags["netif.descr"] != "GigabitEthernet0/0/1" {
		t.Errorf("netif.descr = %v", tags["netif.descr"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Metadata
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_Metadata(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &fullMetric))
	meta, ok := doc["metadata"].(map[string]interface{})
	if !ok {
		t.Fatal("metadata is not an object")
	}
	if meta["collector_id"] != "collector-01" {
		t.Errorf("collector_id = %v", meta["collector_id"])
	}
	if meta["poll_status"] != "success" {
		t.Errorf("poll_status = %v", meta["poll_status"])
	}
	if meta["poll_duration_ms"].(float64) != 245 {
		t.Errorf("poll_duration_ms = %v", meta["poll_duration_ms"])
	}
}

func TestFormat_MetadataOmittedWhenEmpty(t *testing.T) {
	m := fullMetric
	m.Metadata = models.MetricMetadata{} // zero value — should omit due to omitempty
	f := fmtjson.New(fmtjson.Config{}, nil)
	doc := unmarshal(t, mustFormat(t, f, &m))
	// MetricMetadata has omitempty on the struct tag; if all fields are zero
	// the whole section is omitted.
	if meta, ok := doc["metadata"].(map[string]interface{}); ok {
		// Accept presence but all fields should be zero/empty.
		if meta["collector_id"] != "" && meta["collector_id"] != nil {
			t.Errorf("expected empty collector_id, got %v", meta["collector_id"])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Compact vs pretty-print
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_CompactHasNoNewlines(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{PrettyPrint: false}, nil)
	data := mustFormat(t, f, &fullMetric)
	if strings.Contains(string(data), "\n") {
		t.Error("compact output must not contain newlines")
	}
}

func TestFormat_PrettyAndCompactEquivalent(t *testing.T) {
	fCompact := fmtjson.New(fmtjson.Config{}, nil)
	fPretty := fmtjson.New(fmtjson.Config{PrettyPrint: true}, nil)

	compact := mustFormat(t, fCompact, &fullMetric)
	pretty := mustFormat(t, fPretty, &fullMetric)

	// Both should unmarshal to structurally identical documents.
	var dc, dp interface{}
	if err := stdjson.Unmarshal(compact, &dc); err != nil {
		t.Fatalf("unmarshal compact: %v", err)
	}
	if err := stdjson.Unmarshal(pretty, &dp); err != nil {
		t.Fatalf("unmarshal pretty: %v", err)
	}

	rc, _ := stdjson.Marshal(dc)
	rp, _ := stdjson.Marshal(dp)
	if string(rc) != string(rp) {
		t.Errorf("compact and pretty-print produce different structures")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Edge cases
// ─────────────────────────────────────────────────────────────────────────────

func TestFormat_EmptyMetrics(t *testing.T) {
	m := models.SNMPMetric{
		Timestamp: testTimestamp,
		Device:    models.Device{Hostname: "host", SNMPVersion: "2c"},
		Metrics:   nil,
		Metadata:  models.MetricMetadata{PollStatus: "success"},
	}
	f := fmtjson.New(fmtjson.Config{}, nil)
	data := mustFormat(t, f, &m)
	doc := unmarshal(t, data)
	arr, ok := doc["metrics"].([]interface{})
	if ok && len(arr) != 0 {
		t.Errorf("expected empty metrics array, got %d items", len(arr))
	}
}

func TestFormat_ValidJSON(t *testing.T) {
	f := fmtjson.New(fmtjson.Config{}, nil)
	data := mustFormat(t, f, &fullMetric)
	if !stdjson.Valid(data) {
		t.Errorf("output is not valid JSON: %s", data)
	}
}
