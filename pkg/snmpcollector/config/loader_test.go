package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"snmp/snmp-collector/pkg/snmpcollector/config"
)

func tmpDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// ── PathsFromEnv ─────────────────────────────────────────────────────────────

func TestPathsFromEnv_Defaults(t *testing.T) {
	for _, v := range []string{
		"INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH",
		"INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH",
		"INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH",
		"INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH",
		"PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH",
	} {
		t.Setenv(v, "")
	}
	p := config.PathsFromEnv()
	if p.Devices != "/etc/snmp_collector/snmp/devices" {
		t.Errorf("Devices = %q", p.Devices)
	}
	if p.Objects != "/etc/snmp_collector/snmp/objects" {
		t.Errorf("Objects = %q", p.Objects)
	}
	if p.Enums != "/etc/snmp_collector/snmp/enums" {
		t.Errorf("Enums = %q", p.Enums)
	}
}

func TestPathsFromEnv_Override(t *testing.T) {
	t.Setenv("INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH", "/custom/devices")
	p := config.PathsFromEnv()
	if p.Devices != "/custom/devices" {
		t.Errorf("Devices = %q, want /custom/devices", p.Devices)
	}
}

// ── Device loading ────────────────────────────────────────────────────────────

var deviceYAML = `
router01.example.com:
  ip: 192.0.2.1
  port: 161
  poll_interval: 60
  timeout: 3000
  retries: 2
  exponential_timeout: false
  version: 2c
  communities:
    - public
  device_groups:
    - cisco_c1000
  max_concurrent_polls: 4

switch01.example.com:
  ip: 192.0.2.2
  version: 3
  v3_credentials:
    - username: snmp_collector
      authentication_protocol: sha
      authentication_passphrase: efauthpassword
      privacy_protocol: des
      privacy_passphrase: efprivpassword
  device_groups:
    - cisco_c1000
`

func TestLoad_Devices(t *testing.T) {
	devDir := tmpDir(t, map[string]string{"devices.yml": deviceYAML})
	cfg, err := config.Load(config.Paths{
		Devices:      devDir,
		DeviceGroups: t.TempDir(), ObjectGroups: t.TempDir(),
		Objects: t.TempDir(), Enums: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Devices) != 2 {
		t.Fatalf("devices count = %d, want 2", len(cfg.Devices))
	}
	r := cfg.Devices["router01.example.com"]
	if r.IP != "192.0.2.1" {
		t.Errorf("ip = %q", r.IP)
	}
	if r.Version != "2c" {
		t.Errorf("version = %q", r.Version)
	}
	if len(r.Communities) != 1 || r.Communities[0] != "public" {
		t.Errorf("communities = %v", r.Communities)
	}
	sw := cfg.Devices["switch01.example.com"]
	if sw.Version != "3" {
		t.Errorf("v3 version = %q", sw.Version)
	}
	if len(sw.V3Credentials) != 1 {
		t.Fatalf("v3_credentials count = %d", len(sw.V3Credentials))
	}
	if sw.V3Credentials[0].Username != "snmp_collector" {
		t.Errorf("username = %q", sw.V3Credentials[0].Username)
	}
	if sw.V3Credentials[0].AuthenticationProtocol != "sha" {
		t.Errorf("auth_protocol = %q", sw.V3Credentials[0].AuthenticationProtocol)
	}
}

// ── Hard-coded fallbacks ──────────────────────────────────────────────────────

var minimalDeviceYAML = `
router02.example.com:
  ip: 10.0.0.1
  version: 2c
  communities:
    - private
  device_groups:
    - generic
`

func TestLoad_FallbacksApplied(t *testing.T) {
	devDir := tmpDir(t, map[string]string{"devices.yml": minimalDeviceYAML})
	cfg, err := config.Load(config.Paths{
		Devices:      devDir,
		DeviceGroups: t.TempDir(), ObjectGroups: t.TempDir(),
		Objects: t.TempDir(), Enums: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Devices["router02.example.com"]
	if d.IP != "10.0.0.1" {
		t.Errorf("ip = %q", d.IP)
	}
	if d.Port != 161 {
		t.Errorf("port = %d, want 161 (hard-coded fallback)", d.Port)
	}
	if d.Timeout != 3000 {
		t.Errorf("timeout = %d, want 3000 (hard-coded fallback)", d.Timeout)
	}
	if d.Retries != 2 {
		t.Errorf("retries = %d, want 2 (hard-coded fallback)", d.Retries)
	}
	if d.MaxConcurrentPolls != 4 {
		t.Errorf("max_concurrent_polls = %d, want 4 (hard-coded fallback)", d.MaxConcurrentPolls)
	}
	if d.PollInterval != 60 {
		t.Errorf("poll_interval = %d, want 60 (hard-coded fallback)", d.PollInterval)
	}
}

// ── Device groups ─────────────────────────────────────────────────────────────

var deviceGroupYAML = `
cisco_c1000:
  object_groups:
    - system
    - host
    - netif
generic:
  object_groups:
    - system
    - netif
`

func TestLoad_DeviceGroups(t *testing.T) {
	dgDir := tmpDir(t, map[string]string{"groups.yml": deviceGroupYAML})
	cfg, err := config.Load(config.Paths{
		Devices:      t.TempDir(),
		DeviceGroups: dgDir, ObjectGroups: t.TempDir(),
		Objects: t.TempDir(), Enums: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cisco, ok := cfg.DeviceGroups["cisco_c1000"]
	if !ok {
		t.Fatal("cisco_c1000 group not found")
	}
	if len(cisco.ObjectGroups) != 3 {
		t.Errorf("object_groups count = %d, want 3", len(cisco.ObjectGroups))
	}
}

// ── Object groups ─────────────────────────────────────────────────────────────

var objectGroupYAML = `
netif:
  objects:
    - IF-MIB::ifEntry
    - IF-MIB::ifXEntry
`

func TestLoad_ObjectGroups(t *testing.T) {
	ogDir := tmpDir(t, map[string]string{"netif.yml": objectGroupYAML})
	cfg, err := config.Load(config.Paths{
		Devices:      t.TempDir(),
		DeviceGroups: t.TempDir(), ObjectGroups: ogDir,
		Objects: t.TempDir(), Enums: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	netif, ok := cfg.ObjectGroups["netif"]
	if !ok {
		t.Fatal("netif group not found")
	}
	if len(netif.Objects) != 2 {
		t.Errorf("objects count = %d, want 2", len(netif.Objects))
	}
}

// ── Object definitions ────────────────────────────────────────────────────────

var ifEntryYAML = `
IF-MIB::ifEntry:
  mib: IF-MIB
  object: ifEntry
  index:
    - type: Integer
      oid: .1.3.6.1.2.1.2.2.1.1
      name: netif
      syntax: InterfaceIndex
  discovery_attribute: ifDescr
  attributes:
    ifDescr:
      oid: .1.3.6.1.2.1.2.2.1.2
      tag: true
      name: netif.descr
      syntax: DisplayString
    ifOperStatus:
      oid: .1.3.6.1.2.1.2.2.1.8
      name: netif.state.oper
      syntax: EnumInteger
    ifInOctets:
      oid: .1.3.6.1.2.1.2.2.1.10
      name: netif.bytes.in
      syntax: Counter32
`

var ifXEntryYAML = `
IF-MIB::ifXEntry:
  mib: IF-MIB
  object: ifXEntry
  augments: IF-MIB::ifEntry
  attributes:
    ifHCInOctets:
      oid: .1.3.6.1.2.1.31.1.1.1.6
      name: netif.bytes.in
      syntax: Counter64
      overrides:
        object: IF-MIB::ifEntry
        attribute: ifInOctets
`

func TestLoad_ObjectDefs(t *testing.T) {
	objDir := tmpDir(t, map[string]string{
		"ifEntry.yml":  ifEntryYAML,
		"ifXEntry.yml": ifXEntryYAML,
	})
	cfg, err := config.Load(config.Paths{
		Devices:      t.TempDir(),
		DeviceGroups: t.TempDir(), ObjectGroups: t.TempDir(),
		Objects: objDir, Enums: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ObjectDefs) != 2 {
		t.Fatalf("object defs count = %d, want 2", len(cfg.ObjectDefs))
	}
	ifEntry, ok := cfg.ObjectDefs["IF-MIB::ifEntry"]
	if !ok {
		t.Fatal("IF-MIB::ifEntry not found")
	}
	if ifEntry.MIB != "IF-MIB" {
		t.Errorf("mib = %q", ifEntry.MIB)
	}
	if ifEntry.DiscoveryAttribute != "ifDescr" {
		t.Errorf("discovery_attribute = %q", ifEntry.DiscoveryAttribute)
	}
	if len(ifEntry.Index) != 1 {
		t.Fatalf("index count = %d, want 1", len(ifEntry.Index))
	}
	if ifEntry.Index[0].Type != "Integer" {
		t.Errorf("index type = %q", ifEntry.Index[0].Type)
	}
	// Leading dot must be stripped.
	if ifEntry.Index[0].OID != "1.3.6.1.2.1.2.2.1.1" {
		t.Errorf("index OID = %q, want without leading dot", ifEntry.Index[0].OID)
	}
	descr, ok := ifEntry.Attributes["ifDescr"]
	if !ok {
		t.Fatal("ifDescr attribute not found")
	}
	if !descr.IsTag {
		t.Error("ifDescr should be a tag")
	}
	if descr.Name != "netif.descr" {
		t.Errorf("name = %q", descr.Name)
	}
	if descr.OID != "1.3.6.1.2.1.2.2.1.2" {
		t.Errorf("oid = %q (want without leading dot)", descr.OID)
	}
	ifXEntry := cfg.ObjectDefs["IF-MIB::ifXEntry"]
	hcIn, ok := ifXEntry.Attributes["ifHCInOctets"]
	if !ok {
		t.Fatal("ifHCInOctets not found")
	}
	if hcIn.Overrides == nil {
		t.Fatal("overrides must not be nil")
	}
	if hcIn.Overrides.Object != "IF-MIB::ifEntry" {
		t.Errorf("overrides.object = %q", hcIn.Overrides.Object)
	}
	if hcIn.Overrides.Attribute != "ifInOctets" {
		t.Errorf("overrides.attribute = %q", hcIn.Overrides.Attribute)
	}
	if ifXEntry.Augments != "IF-MIB::ifEntry" {
		t.Errorf("augments = %q", ifXEntry.Augments)
	}
}

// ── Enum definitions ──────────────────────────────────────────────────────────

var intEnumYAML = `
.1.3.6.1.2.1.2.2.1.8:
  1: 'up'
  2: 'down'
  3: 'testing'
`

var oidEnumYAML = `
.1.3.6.1.2.1.25.2.1.1: 'other'
.1.3.6.1.2.1.25.2.1.2: 'RAM'
.1.3.6.1.2.1.25.2.1.4: 'fixed disk'
`

var bitmapEnumYAML = `
.1.3.6.1.2.1.10.166.3.2.10.1.5:
  0: 'PDR'
  1: 'PBS'
  2: 'CDR'
  3: 'CBS'
`

var bitmapAttrObjYAML = `
MPLS-TE-STD-MIB::mplsTunnelCRLDP:
  mib: MPLS-TE-STD-MIB
  object: mplsTunnelCRLDP
  attributes:
    mplsTunnelCRLDPResFlags:
      oid: .1.3.6.1.2.1.10.166.3.2.10.1.5
      name: mpls.crldp.flags
      syntax: EnumBitmap
`

func TestLoad_IntegerEnum(t *testing.T) {
	enumDir := tmpDir(t, map[string]string{"ifOperStatus.yml": intEnumYAML})
	cfg, err := config.Load(config.Paths{
		Devices:      t.TempDir(),
		DeviceGroups: t.TempDir(), ObjectGroups: t.TempDir(),
		Objects: t.TempDir(), Enums: enumDir,
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Enums.Resolve("1.3.6.1.2.1.2.2.1.8", int64(1))
	if got != "up" {
		t.Errorf("Resolve(1) = %v, want %q", got, "up")
	}
	got = cfg.Enums.Resolve("1.3.6.1.2.1.2.2.1.8", int64(2))
	if got != "down" {
		t.Errorf("Resolve(2) = %v, want %q", got, "down")
	}
	got = cfg.Enums.Resolve("1.3.6.1.2.1.2.2.1.8", int64(99))
	if got != int64(99) {
		t.Errorf("Resolve(99) = %v, want passthrough", got)
	}
}

func TestLoad_OIDEnum(t *testing.T) {
	enumDir := tmpDir(t, map[string]string{"hrStorageType.yml": oidEnumYAML})
	cfg, err := config.Load(config.Paths{
		Devices:      t.TempDir(),
		DeviceGroups: t.TempDir(), ObjectGroups: t.TempDir(),
		Objects: t.TempDir(), Enums: enumDir,
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Enums.Resolve("unused", "1.3.6.1.2.1.25.2.1.2")
	if got != "RAM" {
		t.Errorf("OID enum Resolve = %v, want %q", got, "RAM")
	}
}

func TestLoad_BitmapEnum(t *testing.T) {
	objDir := tmpDir(t, map[string]string{"mpls.yml": bitmapAttrObjYAML})
	enumDir := tmpDir(t, map[string]string{"crldp.yml": bitmapEnumYAML})
	cfg, err := config.Load(config.Paths{
		Devices:      t.TempDir(),
		DeviceGroups: t.TempDir(), ObjectGroups: t.TempDir(),
		Objects: objDir, Enums: enumDir,
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Bits 0 and 2 set (mask = 5 = 0b101) -> "PDR,CDR"
	got := cfg.Enums.Resolve("1.3.6.1.2.1.10.166.3.2.10.1.5", int64(5))
	if got != "PDR,CDR" {
		t.Errorf("bitmap Resolve(5) = %v, want %q", got, "PDR,CDR")
	}
}

// ── Missing directories ───────────────────────────────────────────────────────

func TestLoad_MissingDirectoriesAreIgnored(t *testing.T) {
	_, err := config.Load(config.Paths{
		Devices:      "/tmp/no-such-devices",
		DeviceGroups: "/tmp/no-such-dg",
		ObjectGroups: "/tmp/no-such-og",
		Objects:      "/tmp/no-such-objects",
		Enums:        "/tmp/no-such-enums",
	}, nil)
	if err != nil {
		t.Errorf("missing dirs should not cause error, got: %v", err)
	}
}

// ── Multiple files ────────────────────────────────────────────────────────────

func TestLoad_MultipleObjectFiles(t *testing.T) {
	objDir := tmpDir(t, map[string]string{
		"ifEntry.yml":  ifEntryYAML,
		"ifXEntry.yml": ifXEntryYAML,
	})
	cfg, err := config.Load(config.Paths{
		Devices:      t.TempDir(),
		DeviceGroups: t.TempDir(), ObjectGroups: t.TempDir(),
		Objects: objDir, Enums: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ObjectDefs) != 2 {
		t.Errorf("expected 2 object defs, got %d", len(cfg.ObjectDefs))
	}
}

// ── Full integration ──────────────────────────────────────────────────────────

func TestLoad_FullIntegration(t *testing.T) {
	devDir := tmpDir(t, map[string]string{"router01.yml": deviceYAML})
	dgDir := tmpDir(t, map[string]string{"groups.yml": deviceGroupYAML})
	ogDir := tmpDir(t, map[string]string{"netif.yml": objectGroupYAML})
	objDir := tmpDir(t, map[string]string{"ifEntry.yml": ifEntryYAML})
	enumDir := tmpDir(t, map[string]string{"ifOperStatus.yml": intEnumYAML})

	cfg, err := config.Load(config.Paths{
		Devices:      devDir,
		DeviceGroups: dgDir, ObjectGroups: ogDir,
		Objects: objDir, Enums: enumDir,
	}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Devices) == 0 {
		t.Error("expected devices")
	}
	if len(cfg.DeviceGroups) == 0 {
		t.Error("expected device groups")
	}
	if len(cfg.ObjectGroups) == 0 {
		t.Error("expected object groups")
	}
	if len(cfg.ObjectDefs) == 0 {
		t.Error("expected object defs")
	}
	if cfg.Enums == nil {
		t.Error("expected enum registry")
	}
}
