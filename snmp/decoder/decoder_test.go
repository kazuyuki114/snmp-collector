package decoder_test

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
	"snmp/snmp-collector/models"
	"snmp/snmp-collector/snmp/decoder"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared fixtures
// ─────────────────────────────────────────────────────────────────────────────

// ifEntryDef mirrors the IF-MIB::ifEntry fragment from the architecture doc.
var ifEntryDef = models.ObjectDefinition{
	Key:    "IF-MIB::ifEntry",
	MIB:    "IF-MIB",
	Object: "ifEntry",
	Attributes: map[string]models.AttributeDefinition{
		"ifDescr": {
			OID:    ".1.3.6.1.2.1.2.2.1.2",
			Name:   "netif.descr",
			Syntax: "DisplayString",
			IsTag:  true,
		},
		"ifType": {
			OID:    ".1.3.6.1.2.1.2.2.1.3",
			Name:   "netif.type",
			Syntax: "EnumInteger",
			IsTag:  true,
		},
		"ifSpeed": {
			OID:    ".1.3.6.1.2.1.2.2.1.5",
			Name:   "netif.bandwidth.bw",
			Syntax: "Gauge32",
		},
		"ifOperStatus": {
			OID:    ".1.3.6.1.2.1.2.2.1.8",
			Name:   "netif.state.oper",
			Syntax: "EnumInteger",
		},
		"ifInOctets": {
			OID:    ".1.3.6.1.2.1.2.2.1.10",
			Name:   "netif.bytes.in",
			Syntax: "Counter32",
		},
		"ifOutOctets": {
			OID:    ".1.3.6.1.2.1.2.2.1.16",
			Name:   "netif.bytes.out",
			Syntax: "Counter32",
		},
	},
}

// testPDUs simulates a GetBulk response for two interfaces (index 1 and 2).
var testPDUs = []gosnmp.SnmpPDU{
	// Interface 1
	{Name: ".1.3.6.1.2.1.2.2.1.2.1", Type: gosnmp.OctetString, Value: []byte("GigabitEthernet0/0/1")},
	{Name: ".1.3.6.1.2.1.2.2.1.3.1", Type: gosnmp.Integer, Value: 6},                // ethernetCsmacd
	{Name: ".1.3.6.1.2.1.2.2.1.5.1", Type: gosnmp.Gauge32, Value: uint(1000000000)}, // 1 Gbps
	{Name: ".1.3.6.1.2.1.2.2.1.8.1", Type: gosnmp.Integer, Value: 1},                // up
	{Name: ".1.3.6.1.2.1.2.2.1.10.1", Type: gosnmp.Counter32, Value: uint(1234567890)},
	{Name: ".1.3.6.1.2.1.2.2.1.16.1", Type: gosnmp.Counter32, Value: uint(987654321)},
	// Interface 2
	{Name: ".1.3.6.1.2.1.2.2.1.2.2", Type: gosnmp.OctetString, Value: []byte("GigabitEthernet0/0/2")},
	{Name: ".1.3.6.1.2.1.2.2.1.5.2", Type: gosnmp.Gauge32, Value: uint(1000000000)},
	{Name: ".1.3.6.1.2.1.2.2.1.8.2", Type: gosnmp.Integer, Value: 2}, // down
	{Name: ".1.3.6.1.2.1.2.2.1.10.2", Type: gosnmp.Counter32, Value: uint(5678901234)},
	{Name: ".1.3.6.1.2.1.2.2.1.16.2", Type: gosnmp.Counter32, Value: uint(4321098765)},
	// Trailing OID beyond our object tree (normal in bulk walk) — must be ignored.
	{Name: ".1.3.6.1.2.1.2.2.1.99.1", Type: gosnmp.Integer, Value: 0},
	// Error sentinel — must be skipped.
	{Name: ".1.3.6.1.2.1.2.2.1.10.3", Type: gosnmp.NoSuchInstance, Value: nil},
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ─────────────────────────────────────────────────────────────────────────────
// VarbindParser tests
// ─────────────────────────────────────────────────────────────────────────────

func TestVarbindParser_Parse_CorrectCount(t *testing.T) {
	p, err := decoder.NewVarbindParser(ifEntryDef)
	if err != nil {
		t.Fatalf("NewVarbindParser: %v", err)
	}

	got, err := p.Parse(testPDUs)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// 13 PDUs total; 1 is out-of-tree (.99.1), 1 is NoSuchInstance (.10.3) → expect 11 decoded.
	const want = 11
	if len(got) != want {
		t.Errorf("decoded count = %d, want %d", len(got), want)
	}
}

func TestVarbindParser_Parse_InstanceExtraction(t *testing.T) {
	p, err := decoder.NewVarbindParser(ifEntryDef)
	if err != nil {
		t.Fatalf("NewVarbindParser: %v", err)
	}

	got, err := p.Parse(testPDUs)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Build a quick lookup: [attributeName][instance] → value
	type key struct{ name, instance string }
	results := make(map[key]interface{}, len(got))
	for _, v := range got {
		results[key{v.AttributeName, v.Instance}] = v.Value
	}

	tests := []struct {
		name     string
		instance string
		wantVal  interface{}
	}{
		{"netif.descr", "1", "GigabitEthernet0/0/1"},
		{"netif.descr", "2", "GigabitEthernet0/0/2"},
		{"netif.state.oper", "1", int64(1)},
		{"netif.state.oper", "2", int64(2)},
		{"netif.bytes.in", "1", uint64(1234567890)},
		{"netif.bytes.out", "2", uint64(4321098765)},
	}

	for _, tc := range tests {
		k := key{tc.name, tc.instance}
		val, ok := results[k]
		if !ok {
			t.Errorf("missing result for attribute=%q instance=%q", tc.name, tc.instance)
			continue
		}
		if val != tc.wantVal {
			t.Errorf("attribute=%q instance=%q: got %v (%T), want %v (%T)",
				tc.name, tc.instance, val, val, tc.wantVal, tc.wantVal)
		}
	}
}

func TestVarbindParser_Parse_TagAttributesFlagged(t *testing.T) {
	p, err := decoder.NewVarbindParser(ifEntryDef)
	if err != nil {
		t.Fatalf("NewVarbindParser: %v", err)
	}

	got, err := p.Parse(testPDUs)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tagNames := map[string]bool{"netif.descr": true, "netif.type": true}
	for _, v := range got {
		if tagNames[v.AttributeName] && !v.IsTag {
			t.Errorf("attribute %q should be IsTag=true but got false", v.AttributeName)
		}
		if !tagNames[v.AttributeName] && v.IsTag {
			t.Errorf("attribute %q should be IsTag=false but got true", v.AttributeName)
		}
	}
}

func TestVarbindParser_EmptyObjectDef_ReturnsError(t *testing.T) {
	_, err := decoder.NewVarbindParser(models.ObjectDefinition{Key: "EMPTY::entry"})
	if err == nil {
		t.Fatal("expected error for empty ObjectDefinition, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SNMPDecoder tests
// ─────────────────────────────────────────────────────────────────────────────

func TestSNMPDecoder_Decode_HappyPath(t *testing.T) {
	dec := decoder.NewSNMPDecoder(newTestLogger())

	now := time.Now()
	raw := decoder.RawPollResult{
		Device: models.Device{
			Hostname:    "router01.example.com",
			IPAddress:   "192.0.2.1",
			SNMPVersion: "2c",
		},
		ObjectDef:     ifEntryDef,
		Varbinds:      testPDUs,
		CollectedAt:   now,
		PollStartedAt: now.Add(-245 * time.Millisecond),
	}

	result, err := dec.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if result.Device.Hostname != "router01.example.com" {
		t.Errorf("device hostname = %q, want %q", result.Device.Hostname, "router01.example.com")
	}
	if result.ObjectDefKey != "IF-MIB::ifEntry" {
		t.Errorf("object def key = %q, want %q", result.ObjectDefKey, "IF-MIB::ifEntry")
	}
	if result.PollDurationMs < 200 || result.PollDurationMs > 300 {
		t.Errorf("poll duration = %dms, want ~245ms", result.PollDurationMs)
	}
	if len(result.Varbinds) != 11 {
		t.Errorf("varbind count = %d, want 11", len(result.Varbinds))
	}
}

func TestSNMPDecoder_Decode_EmptyVarbinds_NoError(t *testing.T) {
	dec := decoder.NewSNMPDecoder(nil) // nil logger must not panic

	raw := decoder.RawPollResult{
		Device:        models.Device{Hostname: "sw01"},
		ObjectDef:     ifEntryDef,
		Varbinds:      nil,
		CollectedAt:   time.Now(),
		PollStartedAt: time.Now(),
	}

	result, err := dec.Decode(raw)
	if err != nil {
		t.Fatalf("Decode with empty varbinds should not return error, got: %v", err)
	}
	if len(result.Varbinds) != 0 {
		t.Errorf("expected 0 varbinds for empty input, got %d", len(result.Varbinds))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ConvertValue / type conversion tests
// ─────────────────────────────────────────────────────────────────────────────

func TestConvertValue_BandwidthMBits(t *testing.T) {
	// 100 Mbps expressed as raw integer 100 → normalised to 100_000_000 bits/sec
	got, err := decoder.ConvertValue(gosnmp.Gauge32, uint(100), "BandwidthMBits")
	if err != nil {
		t.Fatalf("ConvertValue: %v", err)
	}
	const want float64 = 100_000_000
	if got != want {
		t.Errorf("BandwidthMBits: got %v, want %v", got, want)
	}
}

func TestConvertValue_Counter64(t *testing.T) {
	got, err := decoder.ConvertValue(gosnmp.Counter64, uint64(1<<63), "Counter64")
	if err != nil {
		t.Fatalf("ConvertValue: %v", err)
	}
	if got != uint64(1<<63) {
		t.Errorf("Counter64: got %v, want %v", got, uint64(1<<63))
	}
}

func TestConvertValue_MACAddress(t *testing.T) {
	raw := []byte{0x00, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e}
	got, err := decoder.ConvertValue(gosnmp.OctetString, raw, "MacAddress")
	if err != nil {
		t.Fatalf("ConvertValue: %v", err)
	}
	const want = "00:1a:2b:3c:4d:5e"
	if got != want {
		t.Errorf("MacAddress: got %q, want %q", got, want)
	}
}

func TestConvertValue_IpAddress(t *testing.T) {
	// gosnmp returns IpAddress as a raw 4-byte string.
	raw := string([]byte{192, 0, 2, 1})
	got, err := decoder.ConvertValue(gosnmp.IPAddress, raw, "IpAddress")
	if err != nil {
		t.Fatalf("ConvertValue: %v", err)
	}
	const want = "192.0.2.1"
	if got != want {
		t.Errorf("IpAddress: got %q, want %q", got, want)
	}
}

func TestConvertValue_ErrorType_ReturnsError(t *testing.T) {
	_, err := decoder.ConvertValue(gosnmp.NoSuchObject, nil, "Integer")
	if err == nil {
		t.Fatal("expected error for NoSuchObject type, got nil")
	}
}
