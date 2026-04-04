# ezSNMP — Comprehensive Architecture Report

> Generated from full codebase audit of all Go source files (25+), test files (3), YAML configs, documentation, and `metrics.json` output.

---

## Table of Contents

1. [Module & Dependencies](#1-module--dependencies)
2. [Package Map](#2-package-map)
3. [Interfaces Defined](#3-interfaces-defined)
4. [Data Models & Structs](#4-data-models--structs)
5. [Pipeline Architecture](#5-pipeline-architecture)
6. [Two Pipeline Implementations](#6-two-pipeline-implementations)
7. [Configuration Model](#7-configuration-model)
8. [Component Deep Dive](#8-component-deep-dive)
9. [Plugin System](#9-plugin-system)
10. [Test Coverage](#10-test-coverage)
11. [Implemented vs. Planned (Architecture Spec Comparison)](#11-implemented-vs-planned)
12. [File-by-File Inventory](#12-file-by-file-inventory)

---

## 1. Module & Dependencies

**Module:** `github.com/kazuyuki114/ezSNMP`  
**Go version:** 1.25.7

| Dependency | Version | Purpose |
|---|---|---|
| `github.com/gosnmp/gosnmp` | v1.43.2 | SNMP protocol client (Get/Walk/BulkWalk, v1/v2c/v3) |
| `gopkg.in/yaml.v3` | v3.0.1 | YAML configuration parsing |

The project has **zero framework dependencies** — only gosnmp for SNMP protocol and yaml.v3 for config. All pipeline logic, concurrency, connection pooling, scheduling, and formatting use the Go standard library.

---

## 2. Package Map

```
github.com/kazuyuki114/ezSNMP
├── cmd/snmpcollector/          # CLI entry point binary
├── models/                     # Shared data contracts (zero internal imports)
│   ├── config.go               #   ObjectDefinition, AttributeDefinition, etc.
│   └── metric.go               #   SNMPMetric, Device, Metric, MetricMetadata
├── snmp/decoder/               # Stage 3: SNMP PDU → decoded varbinds
│   ├── decoder.go              #   Decoder interface, SNMPDecoder, message types
│   ├── varbind.go              #   VarbindParser — OID matching, instance extraction
│   └── types.go                #   ConvertValue — 50+ syntax type conversions
├── producer/metrics/           # Stage 4: decoded varbinds → SNMPMetric
│   ├── producer.go             #   Producer interface, MetricsProducer
│   ├── poll.go                 #   Build() — core assembly function
│   ├── normalize.go            #   CounterState — delta computation, wrap detection
│   └── enrich.go               #   EnumRegistry — integer/bitmap/OID enum resolution
├── format/json/                # Stage 5: SNMPMetric → JSON bytes
│   └── formatter.go            #   Formatter interface, JSONFormatter
├── plugin/                     # Plugin system interfaces & engine
│   ├── envelope.go             #   Envelope message type
│   ├── input.go                #   Input interface
│   ├── transport.go            #   Transport interface
│   ├── engine/engine.go        #   Engine — central broker (Input→Format→Transport)
│   ├── input/snmppoller/       #   SNMP Poller as plugin.Input
│   │   └── snmppoller.go
│   └── transport/
│       ├── file/               #   File transport with rotation
│       │   ├── file.go
│       │   └── rotate.go
│       └── stdout/             #   Stdout transport
│           └── stdout.go
└── pkg/snmpcollector/          # Direct pipeline implementation
    ├── app/app.go              #   Full pipeline orchestrator (non-plugin)
    ├── config/
    │   ├── device.go           #   DeviceConfig, V3Credentials, DeviceGroup
    │   └── loader.go           #   YAML config loading from 5 directory trees
    ├── poller/
    │   ├── poller.go           #   Poller interface, SNMPPoller (Get/Walk/BulkWalk)
    │   ├── pool.go             #   ConnectionPool — per-device SNMP session pool
    │   ├── session.go          #   gosnmp session factory
    │   └── worker.go           #   WorkerPool — fan-out job dispatcher
    └── scheduler/
        ├── scheduler.go        #   Timer-based poll job dispatch loop
        └── resolve.go          #   Config hierarchy → flat PollJob list
```

---

## 3. Interfaces Defined

### Core Pipeline Interfaces

| Interface | Package | Methods | Purpose |
|---|---|---|---|
| `Decoder` | `snmp/decoder` | `Decode(RawPollResult) (DecodedPollResult, error)` | SNMP response decoding |
| `Producer` | `producer/metrics` | `Produce(DecodedPollResult) (SNMPMetric, error)` | Metric assembly |
| `Formatter` | `format/json` | `Format(*SNMPMetric) ([]byte, error)` | Output serialization |
| `Poller` | `pkg/.../poller` | `Poll(ctx, PollJob) (RawPollResult, error)` | SNMP polling execution |
| `JobSubmitter` | `pkg/.../scheduler` | `Submit(PollJob)`, `TrySubmit(PollJob) bool` | Abstracts WorkerPool for scheduler |

### Plugin Interfaces

| Interface | Package | Methods | Purpose |
|---|---|---|---|
| `Input` | `plugin` | `Name() string`, `Start(ctx, chan<- Envelope) error`, `Stop()` | Data collection plugin |
| `Transport` | `plugin` | `Name() string`, `Send([]byte) error`, `Close() error` | Output delivery plugin |

---

## 4. Data Models & Structs

### Runtime Data (`models/metric.go`)

```go
// Top-level output — one per device per object per poll cycle
type SNMPMetric struct {
    Timestamp time.Time      `json:"timestamp"`
    Device    Device         `json:"device"`
    Metrics   []Metric       `json:"metrics"`
    Metadata  MetricMetadata `json:"metadata,omitempty"`
}

type Device struct {
    Hostname    string            `json:"hostname"`
    IPAddress   string            `json:"ip_address"`
    SNMPVersion string            `json:"snmp_version"`
    Vendor      string            `json:"vendor,omitempty"`
    Model       string            `json:"model,omitempty"`
    SysDescr    string            `json:"sys_descr,omitempty"`
    SysLocation string            `json:"sys_location,omitempty"`
    SysContact  string            `json:"sys_contact,omitempty"`
    Tags        map[string]string `json:"tags,omitempty"`
}

type Metric struct {
    OID      string            `json:"oid"`
    Name     string            `json:"name"`
    Instance string            `json:"instance,omitempty"`
    Value    interface{}       `json:"value"`
    Type     string            `json:"type"`
    Syntax   string            `json:"syntax"`
    Tags     map[string]string `json:"tags,omitempty"`
}

type MetricMetadata struct {
    CollectorID    string `json:"collector_id,omitempty"`
    PollDurationMs int64  `json:"poll_duration_ms,omitempty"`
    PollStatus     string `json:"poll_status,omitempty"`
}
```

### Configuration Types (`models/config.go`)

```go
type ObjectDefinition struct {
    Key                string
    MIB                string
    Object             string
    Augments           string
    Index              []IndexDefinition
    DiscoveryAttribute string
    Attributes         map[string]AttributeDefinition
}

type AttributeDefinition struct {
    OID        string
    Name       string
    Syntax     string
    IsTag      bool
    Overrides  *OverrideReference
    Rediscover string
}

type OverrideReference struct {
    Object    string
    Attribute string
}
```

### Inter-Stage Message Types

| Type | Package | Flows Between |
|---|---|---|
| `PollJob` | `poller` | Scheduler → WorkerPool |
| `RawPollResult` | `decoder` | Poller → Decoder (contains raw `gosnmp.SnmpPDU` slices) |
| `DecodedPollResult` | `decoder` | Decoder → Producer (contains `[]DecodedVarbind`) |
| `DecodedVarbind` | `decoder` | Internal to DecodedPollResult |
| `SNMPMetric` | `models` | Producer → Formatter |
| `Envelope` | `plugin` | Input → Engine (wraps `*SNMPMetric` with source and timestamp) |

### Device Configuration (`config/device.go`)

```go
type DeviceConfig struct {
    IP                 string
    Port               int
    PollInterval       time.Duration
    Timeout            time.Duration
    Retries            int
    ExponentialTimeout bool
    Version            string
    Communities        []string
    V3Credentials      *V3Credentials
    DeviceGroups       []string
    MaxConcurrentPolls int
}
```

---

## 5. Pipeline Architecture

The collector implements a **6-stage concurrent pipeline** connected by buffered Go channels:

```
                    ┌─────────────────────────────────────────────────────────────────┐
                    │                          ezSNMP Pipeline                        │
                    │                                                                  │
 YAML Config ──▶   │  [1] Scheduler  ──▶  [2] WorkerPool/Poller  ──▶  [rawCh]       │
 (5 dirs)          │       │ PollJobs        │ gosnmp Get/Walk        │               │
                    │       ▼                  ▼                        ▼               │
                    │                                           [3] Decoder            │
                    │                                                │ VarbindParser   │
                    │                                                │ ConvertValue    │
                    │                                                ▼                  │
                    │                                           [decodedCh]            │
                    │                                                │                  │
                    │                                           [4] Producer           │
                    │                                                │ Override Res.   │
                    │                                                │ Enum Resolve    │
                    │                                                │ Counter Delta   │
                    │                                                ▼                  │
                    │                                           [metricCh]             │
                    │                                                │                  │
                    │                                           [5] Formatter          │
                    │                                                │ JSON Marshal    │
                    │                                                ▼                  │
                    │                                           [formattedCh]          │
                    │                                                │                  │
                    │                                           [6] Transport          │
                    │                                                │ stdout / file   │
                    └────────────────────────────────────────────────┼─────────────────┘
                                                                     ▼
                                                              JSON-lines output
```

### Channel Cascade & Shutdown

Shutdown is orderly, cascading from head to tail:

1. Context cancel → Scheduler stops → WorkerPool drains jobs channel
2. WorkerPool stops → closes `rawCh`
3. Decoder goroutine exits on `rawCh` close → closes `decodedCh`
4. Producer goroutine exits → closes `metricCh`
5. Formatter goroutine exits → closes `formattedCh`
6. Transport goroutine exits
7. Connection pool closed

---

## 6. Two Pipeline Implementations

The codebase contains **two independent pipeline wiring implementations**:

### A. Direct Pipeline (`pkg/snmpcollector/app/app.go`)

- **Used by `cmd/snmpcollector/main.go`** — the actual production binary
- Wires all stages directly with 4 buffered channels (`rawCh`, `decodedCh`, `metricCh`, `formattedCh`)
- Uses an inline `writerTransport` (writes to `io.Writer`, defaults to `os.Stdout`)
- Supports optional file transport via `RotatingFile` from `plugin/transport/file`
- Supports `Reload()` for hot config refresh

```go
type Config struct {
    ConfigPaths          config.Paths
    CollectorID          string
    PollerWorkers        int        // default 500
    BufferSize           int        // default 10000
    PoolOptions          poller.PoolOptions
    EnumEnabled          bool
    CounterDeltaEnabled  bool
    PrettyPrint          bool
    TransportWriter      io.Writer  // nil = os.Stdout
}
```

### B. Plugin Engine (`plugin/engine/engine.go`)

- **Not wired from main.go** — available for future/advanced usage
- Generic Telegraf-style broker: any number of `Input` plugins → `Envelope` channel → format workers → fan-out to any number of `Transport` plugins
- The SNMP Poller Input (`plugin/input/snmppoller`) encapsulates the full Scheduler→WorkerPool→Decoder→Producer pipeline internally
- Supports multiple Inputs and Transports simultaneously

```go
engine := engine.New(engine.Config{BufferSize: 10000, FormatWorkers: 4, PrettyPrint: false}, logger)
engine.AddInput(snmpPollerInput)
engine.AddTransport(stdoutTransport)
engine.AddTransport(fileTransport)
engine.Start(ctx)
```

### Key Difference

| Aspect | Direct (`app.App`) | Plugin Engine |
|---|---|---|
| Entry point | `cmd/snmpcollector/main.go` | Not wired (library) |
| Pipeline channels | 4 explicit channels | 1 envelope channel + internal |
| Multi-input | No | Yes (N inputs → 1 channel) |
| Multi-transport | Single writer | Yes (fan-out to N transports) |
| Hot reload | `app.Reload()` | `snmppoller.Input.Reload()` |

---

## 7. Configuration Model

### 5-Directory YAML Hierarchy

```
devices/           → DeviceConfig (IP, port, interval, version, communities, device_groups)
  └── device_groups/  → DeviceGroup (list of object_groups)
       └── object_groups/ → ObjectGroup (list of objects)
            └── objects/      → ObjectDefinition (MIB, OID, attributes, index)
                 └── enums/       → Enum mappings (integer→label, bitmap→labels, OID→label)
```

### Resolution Chain

```
DeviceConfig.DeviceGroups []string
  └── DeviceGroup.ObjectGroups []string
        └── ObjectGroup.Objects []string
              └── ObjectDefinition (from LoadedConfig.ObjectDefs)
```

`scheduler.ResolveJobs()` walks this chain for every device and produces a flat, deduplicated list of `PollJob` values.

### Config Loading (`config/loader.go`)

```go
type Paths struct {
    Devices      string  // env: INPUT_SNMP_DEVICES_DIRECTORY_PATH
    DeviceGroups string  // env: INPUT_SNMP_DEVICE_GROUPS_DIRECTORY_PATH
    ObjectGroups string  // env: INPUT_SNMP_OBJECT_GROUPS_DIRECTORY_PATH
    Objects      string  // env: INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH
    Enums        string  // env: INPUT_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH
}
```

Defaults to `/etc/snmp_collector/snmp/{devices,device_groups,object_groups,objects,enums}`.

### Device Defaults

| Field | Default |
|---|---|
| Port | 161 |
| PollInterval | 60s |
| Timeout | 3000ms |
| Retries | 2 |
| Version | "2c" |
| MaxConcurrentPolls | 4 |

### YAML Examples

**Device** (`testdata/devices/localhost.yml`):
```yaml
myswitch.lab:
  ip: 127.0.0.1
  port: 161
  poll_interval: 300
  version: 2c
  communities: [public]
  device_groups: [generic]
  max_concurrent_polls: 2
```

**Device Group** (`testdata/device_groups/generic.yml`):
```yaml
generic:
  object_groups:
    - ietf_system
    - ietf_host
    - ietf_netif_ethernet
    - ietf_ip
    - ietf_icmp
    - ietf_tcp
    - ietf_udp
    - ietf_snmp
    # ...
```

**Object Group** (`testdata/object_groups/system.yml`):
```yaml
system:
  objects:
    - "SNMPv2-MIB::system"
```

**Object Definition** (`testdata/objects/system.yml`):
```yaml
SNMPv2-MIB::system:
  mib: SNMPv2-MIB
  object: system
  attributes:
    sysDescr:
      oid: ".1.3.6.1.2.1.1.1"
      name: "sys.descr"
      syntax: "DisplayString"
    sysUpTime:
      oid: ".1.3.6.1.2.1.1.3"
      name: "sys.uptime"
      syntax: "TimeTicks"
    sysName:
      oid: ".1.3.6.1.2.1.1.5"
      name: "sys.name"
      syntax: "DisplayString"
```

### Config Scale

The testdata directory contains:
- **1** device (localhost)
- **40+** device groups (cisco, juniper, f5, fortinet, arista, paloalto, linux, etc.)
- **40+** object groups (organized by vendor)
- **Hundreds** of object definitions (organized by vendor subdirectories)

---

## 8. Component Deep Dive

### 8.1 Entry Point (`cmd/snmpcollector/main.go`, 217 lines)

The `run()` function:
1. Parses CLI flags (log level/fmt, collector ID, worker count, buffer size, pool opts, etc.)
2. Builds structured logger (`slog`)
3. Loads config paths from CLI overrides or env vars via `config.PathsFromEnv()`
4. Optionally creates `RotatingFile` transport for file output
5. Constructs `app.Config` and `app.New(cfg, logger)`
6. Starts with `signal.NotifyContext` for SIGINT/SIGTERM graceful shutdown

### 8.2 Scheduler (`pkg/.../scheduler/`, 253 lines)

**Timer algorithm**: Sort-to-next approach:
1. Sort entries by `nextRun` ascending
2. Sleep until earliest entry's `nextRun`
3. On wake, dispatch all entries where `nextRun ≤ now`
4. Use `TrySubmit()` (non-blocking) — full queue causes warning log, no blocking
5. Advance each fired entry by its `interval`

**Hot reload**: `Reload(newCfg)` — atomic swap protected by mutex. Added devices get `nextRun = now`.

### 8.3 Poller (`pkg/.../poller/`, 683 lines total)

**Operation selection** (`SNMPPoller.Poll()`):

| Condition | SNMP Operation |
|---|---|
| Scalar (no Index) | `gosnmp.Get()` with OID+".0" suffix |
| Table + v1 | `gosnmp.WalkAll()` |
| Table + v2c/v3 | `gosnmp.BulkWalkAll()` |

Walk root OID = `LowestCommonOID()` of all attribute OIDs.

**Connection Pool** (`ConnectionPool`):
- Per-device pools with LIFO idle stack
- Concurrency semaphore per device (sized to `MaxConcurrentPolls`)
- Idle timeout with automatic session replacement
- Custom dialer injection for testing

**Worker Pool** (`WorkerPool`):
- N goroutines (default 500) pulling from a buffered jobs channel
- Failed polls with no varbinds are logged but not forwarded (prevents flooding)
- `Submit()` blocking, `TrySubmit()` non-blocking

### 8.4 Decoder (`snmp/decoder/`, 750 lines total)

**Stateless** — safe for concurrent use from any number of goroutines.

**VarbindParser** — Per-`Decode()` call:
1. Builds `attrByOID` map from `ObjectDefinition.Attributes`
2. For each PDU: skip error types → `matchAttribute()` → `ConvertValue()`
3. `matchAttribute()`: direct OID lookup first, then right-to-left prefix scan for table instance extraction

**ConvertValue** — Massive switch handling 50+ syntax types:
- Integer types: `Integer`, `Integer32`, `Unsigned32`, `Gauge32`
- Counter types: `Counter32`, `Counter64`
- String types: `DisplayString`, `OctetString`
- Binary: `MacAddress` → "AA:BB:CC:DD:EE:FF"
- IP: `IpAddress` → "192.168.1.1"
- Unit-scaled: `BandwidthMBits` → bits/sec, `BytesKB` → bytes, `TemperatureDeciC` → °C
- Physical: Power (Watt/MilliWatt/KiloWatt), Current (Amp/MilliAmp/MicroAmp), Voltage, Frequency
- Time: `TimeTicks`, `TicksSec`
- Percent: `Percent`, `Percent100`, `PercentFloat`

### 8.5 Producer (`producer/metrics/`, 570 lines total)

**Build() function** — 6 steps:
1. **Separate** tag vs metric varbinds by `IsTag`
2. **Group by instance** — `tagsByInstance` and `metricsByInstance` maps
3. **Override resolution** — when two attributes share the same `Name`, the one with higher `syntaxPriority()` wins (Counter64=20 > Counter32=10)
4. **Enum resolution** — if enabled, `EnumRegistry.Resolve()` replaces integer/bitmap/OID values with text labels
5. **Counter delta** — if enabled, `CounterState.Delta()` computes per-interval deltas with wrap detection
6. **Assemble** `[]models.Metric` with tags attached per instance

**EnumRegistry** (3 enum types):
- **Integer**: raw int64 → label string (passthrough if no match)
- **Bitmap**: bitmask → comma-joined labels ("PDR,CDR")
- **OID**: OID string value → label string (leading dot normalized)

**CounterState**:
- Thread-safe with mutex
- First observation seeds baseline, returns `Valid=false`
- Second+ observations compute delta
- Wrap detection: Counter32 wraps at 2³²−1, Counter64 at 2⁶⁴−1
- `Purge(maxAge)` reclaims memory for decommissioned interfaces

### 8.6 Formatter (`format/json/`, 110 lines)

- `Formatter` interface with single `Format(*SNMPMetric) ([]byte, error)` method
- `JSONFormatter` — stateless, concurrent-safe
- Supports compact (production) and pretty-print (debug) modes
- Uses standard `json.Marshal`/`json.MarshalIndent`

### 8.7 Transports

**Stdout** (`plugin/transport/stdout/`, 91 lines):
- Writes to `io.Writer` (default `os.Stdout`) with mutex protection
- Appends configurable newline

**File** (`plugin/transport/file/`, 310 lines):
- Wraps `RotatingFile` — size-based file rotation
- Rotation scheme: `metrics.json` → `.1` → `.2` → ... (configurable max backups)
- Thread-safe with mutex

---

## 9. Plugin System

### Architecture

The plugin system follows the **Telegraf model**:

```
Input₁ ─┐                            ┌─▶ Transport₁
Input₂ ─┤──▶ [envelopeCh] ──▶ Fmt ──┤──▶ Transport₂
InputN ─┘                            └─▶ TransportN
```

### Envelope — Standard Internal Message

```go
type Envelope struct {
    Source    string           // must match Input.Name()
    Timestamp time.Time
    Metric   *models.SNMPMetric
}
```

### Engine (`plugin/engine/engine.go`, 233 lines)

The Engine:
1. Creates a shared `envelopeCh` (buffered)
2. Starts all registered Inputs, each sending to `envelopeCh`
3. Runs N format workers that read envelopes, format to JSON, and fan-out to all Transports
4. Handles graceful shutdown: cancel context → wait for inputs → close envelope channel → wait for formatters

### Existing Plugin Implementations

| Plugin | Type | Package | Description |
|---|---|---|---|
| SNMP Poller | Input | `plugin/input/snmppoller` | Full SNMP polling pipeline (Scheduler→Worker→Decoder→Producer) |
| File | Transport | `plugin/transport/file` | Rotating file output |
| Stdout | Transport | `plugin/transport/stdout` | Standard output |

### Plugin Development

Per `docs/plugin-dev-guide.md`, new plugins:
1. Create a package under `plugin/input/<name>/` or `plugin/transport/<name>/`
2. Implement the `plugin.Input` or `plugin.Transport` interface
3. Use compile-time check: `var _ plugin.Input = (*Input)(nil)`
4. Accept `*slog.Logger` and `Config` struct
5. Use `context.WithCancel` + `sync.WaitGroup` for lifecycle

---

## 10. Test Coverage

### Test Files

| File | Tests | Coverage Area |
|---|---|---|
| `format/json/formatter_test.go` (402 lines) | 15+ tests | Construction, nil input, JSON schema compliance (top-level keys, RFC3339 timestamps, device fields, metrics array structure, value types, tags), metadata, compact vs pretty, edge cases |
| `snmp/decoder/decoder_test.go` (~300 lines) | 10+ tests | VarbindParser (count, instance extraction, tag flagging, empty def error), SNMPDecoder (happy path, empty varbinds), ConvertValue (BandwidthMBits, Counter64, MACAddress, IpAddress, error types) |
| `producer/metrics/producer_test.go` (~500 lines) | 20+ tests | EnumRegistry (integer, bitmap, OID, passthrough), CounterState (first/second observation, Counter32 wrap, purge), Build (metric count, tags, override resolution Counter64>Counter32, enum resolution, counter delta, metadata, empty varbinds), MetricsProducer end-to-end |

All tests use shared IF-MIB::ifEntry fixtures for consistency.

### Documented but Not Found in testdata

The docs reference 14 poller tests and 16 scheduler tests, but the corresponding `_test.go` files were not present in the workspace file listing. They may exist but weren't included in the workspace snapshot, or they may be planned.

---

## 11. Implemented vs. Planned (Architecture Spec Comparison)

Comparing `ezSNMP-Architecture.md` (the original spec) against actual code:

### Fully Implemented

| Component | Spec | Code |
|---|---|---|
| SNMP Poller | ✅ Scheduler, Get/Walk/BulkWalk, connection pool | `pkg/snmpcollector/poller/`, `scheduler/` |
| SNMP Decoder | ✅ PDU parsing, type conversion, varbind matching | `snmp/decoder/` |
| Producer | ✅ Normalize, enrich (enum), counter delta, override resolution | `producer/metrics/` |
| JSON Formatter | ✅ Compact + pretty, full schema | `format/json/` |
| Stdout Transport | ✅ | `plugin/transport/stdout/` |
| File Transport | ✅ With size-based rotation | `plugin/transport/file/` |
| Plugin Architecture | ✅ Input + Transport interfaces, Engine | `plugin/` |
| YAML Config Hierarchy | ✅ 5-directory model with env var paths | `pkg/snmpcollector/config/` |
| SNMPv1/v2c/v3 | ✅ Including USM auth/priv | `pkg/snmpcollector/poller/session.go` |
| Hot Reload | ✅ Scheduler.Reload() and App.Reload() | Both pipeline implementations |
| Graceful Shutdown | ✅ Signal handling, cascading channel close | `cmd/snmpcollector/main.go`, `app.go` |
| Connection Pool | ✅ Per-device, LIFO, idle timeout, concurrency limit | `pkg/snmpcollector/poller/pool.go` |

### Not Implemented (Planned in Spec)

| Component | Spec Description | Status |
|---|---|---|
| **Trap Listener** | UDP port 162, v1/v2c/v3 trap parsing, inform ACK | **Not implemented** — no `snmp/trap/`, no `pkg/snmpcollector/trapreceiver/`, no `cmd/trapd/` |
| **MIB Parser** | ASN.1 MIB file parser, OID tree, resolver | **Not implemented** — no `mibs/parser/`, `mibs/registry/`, `mibs/compiler/` |
| **MIB Tool** | `cmd/mibtool/` CLI utility | **Not implemented** |
| **Kafka Transport** | Kafka output plugin | **Not implemented** — spec describes it, dev guide has example skeleton |
| **Protobuf Formatter** | Binary serialization format | **Not implemented** — `format/json/` only |
| **OpenMetrics/Prometheus Formatter** | Prometheus exposition format | **Not implemented** |
| **Time Series Producer** | `producer/timeseries/` (Prometheus, InfluxDB, OpenTSDB) | **Not implemented** |
| **Event Producer** | `producer/event/` (trap→alert conversion, severity mapping) | **Not implemented** |
| **Aggregate Producer** | `producer/metrics/aggregate.go` | **Not implemented** |
| **HTTP Server** | `pkg/snmpcollector/httpserver/` — Prometheus metrics, health checks, API | **Not implemented** |
| **Credentials Manager** | `pkg/snmpcollector/credentials/` — key derivation, rotation | **Not implemented** |
| **Custom SNMP Client** | `snmp/client/` with v1/v2c/v3 implementations | **Not implemented** — uses `gosnmp` directly |
| **BER Encoder** | `snmp/decoder/ber.go` | **Not implemented** — not needed with gosnmp |
| **Dynamic MIB Loading** | Runtime MIB file loading | **Not implemented** — uses static YAML definitions |
| **SO_REUSEPORT** | Multi-threaded trap reception | **Not implemented** |
| **Rate Limiting** | Per-device SNMP rate limiting | **Not implemented** (concurrency limited via connection pool semaphore) |
| **Duplicate Trap Detection** | Trap dedup | **Not implemented** |

### Summary

The **polling pipeline is fully implemented and production-ready**: Config → Scheduler → Poller → Decoder → Producer → Formatter → Transport. The **trap collection side is entirely unimplemented**. The MIB tooling layer is also unimplemented — the system relies on hand-authored YAML object definitions instead of dynamic MIB parsing.

---

## 12. File-by-File Inventory

### Go Source Files (26 files)

| File | Package | Lines | Key Exports |
|---|---|---|---|
| `cmd/snmpcollector/main.go` | `main` | 217 | `main()`, `run()` |
| `models/config.go` | `models` | 81 | `ObjectDefinition`, `IndexDefinition`, `AttributeDefinition`, `OverrideReference` |
| `models/metric.go` | `models` | 53 | `SNMPMetric`, `Device`, `Metric`, `MetricMetadata` |
| `plugin/envelope.go` | `plugin` | 50 | `Envelope`, `Valid()` |
| `plugin/input.go` | `plugin` | 45 | `Input` interface |
| `plugin/transport.go` | `plugin` | 40 | `Transport` interface |
| `plugin/engine/engine.go` | `engine` | 233 | `Engine`, `Config`, `New()`, `AddInput()`, `AddTransport()`, `Start()`, `Stop()` |
| `plugin/input/snmppoller/snmppoller.go` | `snmppoller` | 265 | `Input` (implements `plugin.Input`), `Config`, `New()`, `Reload()` |
| `plugin/transport/file/file.go` | `file` | 117 | `Transport` (implements `plugin.Transport`), `Config`, `New()` |
| `plugin/transport/file/rotate.go` | `file` | 193 | `RotatingFile`, `RotateConfig`, `NewRotatingFile()` |
| `plugin/transport/stdout/stdout.go` | `stdout` | 91 | `Transport` (implements `plugin.Transport`), `Config`, `New()` |
| `producer/metrics/producer.go` | `metrics` | 109 | `Producer` interface, `MetricsProducer`, `Config`, `New()`, `Produce()` |
| `producer/metrics/poll.go` | `metrics` | 174 | `Build()`, `BuildOptions` |
| `producer/metrics/normalize.go` | `metrics` | 156 | `CounterState`, `CounterKey`, `DeltaResult`, `Delta()`, `IsCounterSyntax()`, `WrapForSyntax()` |
| `producer/metrics/enrich.go` | `metrics` | 131 | `EnumRegistry`, `IntEnum`, `NewEnumRegistry()`, `RegisterIntEnum()`, `RegisterOIDEnum()`, `Resolve()` |
| `snmp/decoder/decoder.go` | `decoder` | 156 | `Decoder` interface, `SNMPDecoder`, `RawPollResult`, `DecodedPollResult`, `NewSNMPDecoder()`, `Decode()` |
| `snmp/decoder/varbind.go` | `decoder` | 167 | `DecodedVarbind`, `VarbindParser`, `NewVarbindParser()`, `Parse()` |
| `snmp/decoder/types.go` | `decoder` | 427 | `PDUTypeString()`, `IsErrorType()`, `ConvertValue()` |
| `format/json/formatter.go` | `json` | 110 | `Formatter` interface, `JSONFormatter`, `Config`, `New()`, `Format()` |
| `pkg/snmpcollector/app/app.go` | `app` | 419 | `App`, `Config`, `New()`, `Start()`, `Stop()`, `Reload()` |
| `pkg/snmpcollector/config/device.go` | `config` | 84 | `DeviceConfig`, `V3Credentials`, `DeviceGroup`, `ObjectGroup` |
| `pkg/snmpcollector/config/loader.go` | `config` | 616 | `Paths`, `PathsFromEnv()`, `LoadedConfig`, `Load()` |
| `pkg/snmpcollector/poller/poller.go` | `poller` | 213 | `Poller` interface, `PollJob`, `SNMPPoller`, `Poll()`, `LowestCommonOID()` |
| `pkg/snmpcollector/poller/pool.go` | `poller` | 253 | `ConnectionPool`, `PoolOptions`, `NewConnectionPool()`, `Get()`, `Put()`, `Discard()`, `Close()` |
| `pkg/snmpcollector/poller/session.go` | `poller` | 122 | `NewSession()` |
| `pkg/snmpcollector/poller/worker.go` | `poller` | 95 | `WorkerPool`, `NewWorkerPool()`, `Start()`, `Submit()`, `TrySubmit()`, `Stop()` |
| `pkg/snmpcollector/scheduler/scheduler.go` | `scheduler` | 162 | `Scheduler`, `JobSubmitter` interface, `New()`, `Start()`, `Stop()`, `Reload()`, `Entries()` |
| `pkg/snmpcollector/scheduler/resolve.go` | `scheduler` | 91 | `ResolveJobs()` |

**Total: ~4,600 lines of Go source code** (excluding tests)

### Test Files (3 files)

| File | Lines | Tests |
|---|---|---|
| `format/json/formatter_test.go` | 402 | 15+ |
| `snmp/decoder/decoder_test.go` | ~300 | 10+ |
| `producer/metrics/producer_test.go` | ~500 | 20+ |

### Import Dependency Graph

```
models ← (no imports)
  ↑
  ├── snmp/decoder ← models, gosnmp
  │     ↑
  │     ├── producer/metrics ← models, snmp/decoder
  │     │     ↑
  │     │     ├── format/json ← models
  │     │     │     ↑
  │     │     │     └── plugin/engine ← plugin, format/json
  │     │     │
  │     │     └── plugin/input/snmppoller ← plugin, config, poller, scheduler, decoder, metrics
  │     │
  │     └── pkg/snmpcollector/app ← config, poller, scheduler, decoder, metrics, format/json
  │
  ├── plugin ← models
  │     ├── plugin/engine
  │     ├── plugin/transport/file
  │     └── plugin/transport/stdout
  │
  ├── pkg/snmpcollector/config ← models
  │     ↑
  │     ├── pkg/snmpcollector/poller ← models, config, decoder, gosnmp
  │     │     ↑
  │     │     └── pkg/snmpcollector/scheduler ← config, poller
  │     │
  │     └── cmd/snmpcollector/main ← app, config, poller, file-transport
  │
  └── (all paths converge at cmd/snmpcollector/main.go)
```

### Output Format (`metrics.json`)

The output is **JSON-lines** (one JSON document per line, not a JSON array). Each line is a complete `SNMPMetric` document:

```json
{"timestamp":"2026-03-02T21:42:29.237Z","device":{"hostname":"myswitch.lab","ip_address":"127.0.0.1","snmp_version":"2c"},"metrics":[{"oid":"1.3.6.1.2.1.11.4.0","name":"snmp.msgs.community_unknown.in","instance":"0","value":0,"type":"Counter32","syntax":"Counter32"},...],"metadata":{"collector_id":"LENOVO","poll_duration_ms":0,"poll_status":"success"}}
```

The sample output demonstrates collection of: SNMP statistics, SNMPv3 engine, system info (sysDescr, sysUpTime, sysName), host resources (CPU %, memory, disk), network interfaces (bytes in/out, packets, errors, admin/oper state), IP statistics, TCP/UDP/ICMP counters — all from a single localhost device running snmpd.

---

## Summary

**ezSNMP** is a production-quality SNMP polling collector with:
- A clean 6-stage pipeline (Scheduler → Poller → Decoder → Producer → Formatter → Transport)
- A Telegraf-style plugin system with Input and Transport interfaces
- Comprehensive SNMP v1/v2c/v3 support via gosnmp
- Rich type conversion (50+ SNMP syntax types)
- Counter delta computation with wrap detection
- Enum resolution (integer, bitmap, OID)
- Override resolution (64-bit attributes supersede 32-bit)
- Per-device connection pooling with idle timeout and concurrency control
- Hot-reloadable configuration
- A large testdata library covering Cisco, Juniper, F5, Fortinet, Arista, Linux, and 30+ other vendors

The trap collection, MIB tooling, Kafka transport, Protobuf formatter, HTTP management API, and advanced producers described in the architecture spec are **not yet implemented**.
