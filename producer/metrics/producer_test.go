package metrics_test

import (
	"testing"
	"time"

	"snmp/snmp-collector/models"
	"snmp/snmp-collector/producer/metrics"
	"snmp/snmp-collector/snmp/decoder"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared fixtures
// ─────────────────────────────────────────────────────────────────────────────

var testDevice = models.Device{
	Hostname:    "router01.example.com",
	IPAddress:   "192.0.2.1",
	SNMPVersion: "2c",
}

// ifEntryDecoded is a DecodedPollResult for two interface rows, as the decoder
// would produce from a GetBulk of IF-MIB::ifEntry. It mirrors the architecture
// JSON output example exactly.
func ifEntryDecoded(collectedAt time.Time) decoder.DecodedPollResult {
	return decoder.DecodedPollResult{
		Device:         testDevice,
		ObjectDefKey:   "IF-MIB::ifEntry",
		CollectedAt:    collectedAt,
		PollDurationMs: 245,
		Varbinds: []decoder.DecodedVarbind{
			// ── Interface 1 ─────────────────────────────────────────────────
			{OID: "1.3.6.1.2.1.2.2.1.2.1", AttributeName: "netif.descr", Instance: "1", Value: "GigabitEthernet0/0/1", SNMPType: "OctetString", Syntax: "DisplayString", IsTag: true},
			{OID: "1.3.6.1.2.1.2.2.1.3.1", AttributeName: "netif.type", Instance: "1", Value: int64(6), SNMPType: "Integer", Syntax: "EnumInteger", IsTag: true},
			{OID: "1.3.6.1.2.1.2.2.1.8.1", AttributeName: "netif.state.oper", Instance: "1", Value: int64(1), SNMPType: "Integer", Syntax: "EnumInteger"},
			{OID: "1.3.6.1.2.1.2.2.1.10.1", AttributeName: "netif.bytes.in", Instance: "1", Value: uint64(1234567890), SNMPType: "Counter32", Syntax: "Counter32"},
			{OID: "1.3.6.1.2.1.2.2.1.16.1", AttributeName: "netif.bytes.out", Instance: "1", Value: uint64(987654321), SNMPType: "Counter32", Syntax: "Counter32"},
			// ── Interface 2 ─────────────────────────────────────────────────
			{OID: "1.3.6.1.2.1.2.2.1.2.2", AttributeName: "netif.descr", Instance: "2", Value: "GigabitEthernet0/0/2", SNMPType: "OctetString", Syntax: "DisplayString", IsTag: true},
			{OID: "1.3.6.1.2.1.2.2.1.8.2", AttributeName: "netif.state.oper", Instance: "2", Value: int64(2), SNMPType: "Integer", Syntax: "EnumInteger"},
			{OID: "1.3.6.1.2.1.2.2.1.10.2", AttributeName: "netif.bytes.in", Instance: "2", Value: uint64(5678901234), SNMPType: "Counter32", Syntax: "Counter32"},
		},
	}
}

// findMetric returns the first Metric whose Name and Instance match.
func findMetric(ms []models.Metric, name, instance string) (models.Metric, bool) {
	for _, m := range ms {
		if m.Name == name && m.Instance == instance {
			return m, true
		}
	}
	return models.Metric{}, false
}

// ─────────────────────────────────────────────────────────────────────────────
// EnumRegistry tests
// ─────────────────────────────────────────────────────────────────────────────

func TestEnumRegistry_IntegerEnum(t *testing.T) {
	r := metrics.NewEnumRegistry()
	r.RegisterIntEnum("1.3.6.1.2.1.2.2.1.8", false, map[int64]string{
		1: "up",
		2: "down",
		3: "testing",
	})

	tests := []struct {
		raw  interface{}
		want interface{}
	}{
		{int64(1), "up"},
		{int64(2), "down"},
		{int64(3), "testing"},
		{int64(99), int64(99)}, // no match → passthrough
		{"up", "up"},           // non-int → passthrough
	}

	for _, tc := range tests {
		got := r.Resolve("1.3.6.1.2.1.2.2.1.8", tc.raw)
		if got != tc.want {
			t.Errorf("Resolve(%v) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestEnumRegistry_BitmapEnum(t *testing.T) {
	r := metrics.NewEnumRegistry()
	r.RegisterIntEnum("1.3.6.1.2.1.10.166.3.2.10.1.5", true, map[int64]string{
		0: "PDR",
		1: "PBS",
		2: "CDR",
	})

	// Bits 0 and 2 set → mask = 0b101 = 5
	got := r.Resolve("1.3.6.1.2.1.10.166.3.2.10.1.5", int64(5))
	want := "PDR,CDR"
	if got != want {
		t.Errorf("bitmap Resolve(5) = %v, want %q", got, want)
	}

	// No bits set → raw value returned
	got = r.Resolve("1.3.6.1.2.1.10.166.3.2.10.1.5", int64(0))
	if got != int64(0) {
		t.Errorf("bitmap Resolve(0) = %v, want raw int64(0)", got)
	}
}

func TestEnumRegistry_OIDEnum(t *testing.T) {
	r := metrics.NewEnumRegistry()
	r.RegisterOIDEnum("1.3.6.1.2.1.25.2.1.2", "RAM")
	r.RegisterOIDEnum("1.3.6.1.2.1.25.2.1.4", "fixed disk")

	got := r.Resolve("unused", "1.3.6.1.2.1.25.2.1.2")
	if got != "RAM" {
		t.Errorf("OID enum: got %v, want %q", got, "RAM")
	}

	// Leading dot should still match after normalisation.
	got = r.Resolve("unused", ".1.3.6.1.2.1.25.2.1.4")
	if got != "fixed disk" {
		t.Errorf("OID enum with leading dot: got %v, want %q", got, "fixed disk")
	}
}

func TestEnumRegistry_NoMatchPassthrough(t *testing.T) {
	r := metrics.NewEnumRegistry()
	raw := int64(42)
	got := r.Resolve("1.2.3.4.5", raw)
	if got != raw {
		t.Errorf("expected passthrough %v, got %v", raw, got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CounterState tests
// ─────────────────────────────────────────────────────────────────────────────

func TestCounterState_FirstObservationInvalid(t *testing.T) {
	cs := metrics.NewCounterState()
	key := metrics.CounterKey{Device: "router01", Attribute: "netif.bytes.in", Instance: "1"}
	dr := cs.Delta(key, 1000, time.Now(), metrics.WrapForSyntax("Counter32"))
	if dr.Valid {
		t.Error("first observation should return Valid=false")
	}
}

func TestCounterState_SecondObservationValid(t *testing.T) {
	cs := metrics.NewCounterState()
	key := metrics.CounterKey{Device: "router01", Attribute: "netif.bytes.in", Instance: "1"}

	t0 := time.Now()
	cs.Delta(key, 1000, t0, metrics.WrapForSyntax("Counter32")) // seed

	t1 := t0.Add(60 * time.Second)
	dr := cs.Delta(key, 1500, t1, metrics.WrapForSyntax("Counter32"))

	if !dr.Valid {
		t.Fatal("second observation should be Valid=true")
	}
	if dr.Delta != 500 {
		t.Errorf("delta = %d, want 500", dr.Delta)
	}
	if dr.Elapsed != 60*time.Second {
		t.Errorf("elapsed = %v, want 60s", dr.Elapsed)
	}
}

func TestCounterState_Counter32Wrap(t *testing.T) {
	cs := metrics.NewCounterState()
	key := metrics.CounterKey{Device: "sw01", Attribute: "netif.bytes.out", Instance: "3"}

	const maxU32 = uint64(^uint32(0)) // 4294967295
	t0 := time.Now()
	cs.Delta(key, maxU32-100, t0, maxU32) // last = 4294967195

	t1 := t0.Add(60 * time.Second)
	dr := cs.Delta(key, 400, t1, maxU32) // wrapped: 400 < last

	if !dr.Valid {
		t.Fatal("expected Valid=true after wrap")
	}
	// Expected delta: (maxU32 - (maxU32-100)) + 400 + 1 = 100 + 400 + 1 = 501
	if dr.Delta != 501 {
		t.Errorf("wrap delta = %d, want 501", dr.Delta)
	}
}

func TestCounterState_Purge(t *testing.T) {
	cs := metrics.NewCounterState()
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now()

	cs.Delta(metrics.CounterKey{Device: "a", Attribute: "x", Instance: "1"}, 100, old, ^uint64(0))
	cs.Delta(metrics.CounterKey{Device: "b", Attribute: "x", Instance: "1"}, 100, recent, ^uint64(0))

	removed := cs.Purge(1*time.Hour, time.Now())
	if removed != 1 {
		t.Errorf("purge: removed %d entries, want 1", removed)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Build (poll assembly) tests
// ─────────────────────────────────────────────────────────────────────────────

func TestBuild_MetricCount(t *testing.T) {
	decoded := ifEntryDecoded(time.Now())
	result := metrics.Build(decoded, metrics.BuildOptions{CollectorID: "c1", PollStatus: "success"})

	// 8 varbinds total: 3 tags (descr×2, type×1) → 5 metrics
	if len(result.Metrics) != 5 {
		t.Errorf("metric count = %d, want 5", len(result.Metrics))
	}
}

func TestBuild_TagsAttachedToMetrics(t *testing.T) {
	decoded := ifEntryDecoded(time.Now())
	result := metrics.Build(decoded, metrics.BuildOptions{PollStatus: "success"})

	m, ok := findMetric(result.Metrics, "netif.bytes.in", "1")
	if !ok {
		t.Fatal("metric netif.bytes.in instance=1 not found")
	}
	if m.Tags["netif.descr"] != "GigabitEthernet0/0/1" {
		t.Errorf("tag netif.descr = %q, want %q", m.Tags["netif.descr"], "GigabitEthernet0/0/1")
	}
}

func TestBuild_OverrideResolution_PreferencesHigherSyntax(t *testing.T) {
	// Supply both Counter32 and Counter64 for the same attribute name + instance.
	// Counter64 must win.
	decoded := decoder.DecodedPollResult{
		Device:       testDevice,
		ObjectDefKey: "IF-MIB::ifXEntry",
		CollectedAt:  time.Now(),
		Varbinds: []decoder.DecodedVarbind{
			{OID: "1.3.6.1.2.1.2.2.1.10.1", AttributeName: "netif.bytes.in", Instance: "1", Value: uint64(1000), SNMPType: "Counter32", Syntax: "Counter32"},
			{OID: "1.3.6.1.2.1.31.1.1.1.6.1", AttributeName: "netif.bytes.in", Instance: "1", Value: uint64(9999999999), SNMPType: "Counter64", Syntax: "Counter64"},
		},
	}

	result := metrics.Build(decoded, metrics.BuildOptions{PollStatus: "success"})

	if len(result.Metrics) != 1 {
		t.Fatalf("expected 1 metric after override resolution, got %d", len(result.Metrics))
	}
	m := result.Metrics[0]
	if m.Syntax != "Counter64" {
		t.Errorf("winning syntax = %q, want Counter64", m.Syntax)
	}
	if m.Value != uint64(9999999999) {
		t.Errorf("winning value = %v, want 9999999999", m.Value)
	}
}

func TestBuild_EnumResolution(t *testing.T) {
	reg := metrics.NewEnumRegistry()
	reg.RegisterIntEnum("1.3.6.1.2.1.2.2.1.8", false, map[int64]string{1: "up", 2: "down"})

	decoded := ifEntryDecoded(time.Now())
	result := metrics.Build(decoded, metrics.BuildOptions{
		PollStatus: "success",
		Enums:      reg,
	})

	m, ok := findMetric(result.Metrics, "netif.state.oper", "1")
	if !ok {
		t.Fatal("metric netif.state.oper instance=1 not found")
	}
	if m.Value != "up" {
		t.Errorf("enum resolved value = %v, want %q", m.Value, "up")
	}
}

func TestBuild_CounterDelta_FirstPollZero(t *testing.T) {
	cs := metrics.NewCounterState()
	decoded := ifEntryDecoded(time.Now())

	result := metrics.Build(decoded, metrics.BuildOptions{
		CollectorID: "c1",
		PollStatus:  "success",
		Counters:    cs,
	})

	m, ok := findMetric(result.Metrics, "netif.bytes.in", "1")
	if !ok {
		t.Fatal("metric netif.bytes.in instance=1 not found")
	}
	// First poll: delta is seeded, value emitted as 0.
	if m.Value != uint64(0) {
		t.Errorf("first-poll counter value = %v, want 0", m.Value)
	}
}

func TestBuild_CounterDelta_SecondPollDelta(t *testing.T) {
	cs := metrics.NewCounterState()
	t0 := time.Now()

	// First poll — seeds the counter state.
	decoded1 := ifEntryDecoded(t0)
	metrics.Build(decoded1, metrics.BuildOptions{PollStatus: "success", Counters: cs})

	// Second poll — 60 seconds later, interface 1 bytes.in increases by 6000.
	t1 := t0.Add(60 * time.Second)
	decoded2 := decoder.DecodedPollResult{
		Device:         testDevice,
		ObjectDefKey:   "IF-MIB::ifEntry",
		CollectedAt:    t1,
		PollDurationMs: 210,
		Varbinds: []decoder.DecodedVarbind{
			{OID: "1.3.6.1.2.1.2.2.1.2.1", AttributeName: "netif.descr", Instance: "1", Value: "GigabitEthernet0/0/1", SNMPType: "OctetString", Syntax: "DisplayString", IsTag: true},
			{OID: "1.3.6.1.2.1.2.2.1.10.1", AttributeName: "netif.bytes.in", Instance: "1", Value: uint64(1234567890 + 6000), SNMPType: "Counter32", Syntax: "Counter32"},
		},
	}
	result2 := metrics.Build(decoded2, metrics.BuildOptions{PollStatus: "success", Counters: cs})

	m, ok := findMetric(result2.Metrics, "netif.bytes.in", "1")
	if !ok {
		t.Fatal("metric netif.bytes.in instance=1 not found in second poll")
	}
	if m.Value != uint64(6000) {
		t.Errorf("counter delta = %v, want 6000", m.Value)
	}
}

func TestBuild_Metadata(t *testing.T) {
	decoded := ifEntryDecoded(time.Now())
	result := metrics.Build(decoded, metrics.BuildOptions{
		CollectorID: "collector-01",
		PollStatus:  "success",
	})

	if result.Metadata.CollectorID != "collector-01" {
		t.Errorf("collector_id = %q, want %q", result.Metadata.CollectorID, "collector-01")
	}
	if result.Metadata.PollDurationMs != 245 {
		t.Errorf("poll_duration_ms = %d, want 245", result.Metadata.PollDurationMs)
	}
	if result.Metadata.PollStatus != "success" {
		t.Errorf("poll_status = %q, want %q", result.Metadata.PollStatus, "success")
	}
}

func TestBuild_EmptyVarbinds(t *testing.T) {
	decoded := decoder.DecodedPollResult{
		Device:      testDevice,
		CollectedAt: time.Now(),
		Varbinds:    nil,
	}
	result := metrics.Build(decoded, metrics.BuildOptions{PollStatus: "success"})
	if len(result.Metrics) != 0 {
		t.Errorf("expected 0 metrics for empty varbinds, got %d", len(result.Metrics))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// MetricsProducer end-to-end test
// ─────────────────────────────────────────────────────────────────────────────

func TestMetricsProducer_Produce(t *testing.T) {
	reg := metrics.NewEnumRegistry()
	reg.RegisterIntEnum("1.3.6.1.2.1.2.2.1.8", false, map[int64]string{1: "up", 2: "down"})

	p := metrics.New(metrics.Config{
		CollectorID:         "test-collector",
		EnumEnabled:         true,
		Enums:               reg,
		CounterDeltaEnabled: false,
	}, nil)

	decoded := ifEntryDecoded(time.Now())
	result, err := p.Produce(decoded)
	if err != nil {
		t.Fatalf("Produce: %v", err)
	}
	if result.Device.Hostname != "router01.example.com" {
		t.Errorf("device hostname = %q", result.Device.Hostname)
	}
	if result.Metadata.CollectorID != "test-collector" {
		t.Errorf("collector_id = %q", result.Metadata.CollectorID)
	}

	// Enum should have resolved state.oper for interface 1 to "up".
	m, ok := findMetric(result.Metrics, "netif.state.oper", "1")
	if !ok {
		t.Fatal("netif.state.oper instance=1 not found")
	}
	if m.Value != "up" {
		t.Errorf("enum value = %v, want %q", m.Value, "up")
	}
}
