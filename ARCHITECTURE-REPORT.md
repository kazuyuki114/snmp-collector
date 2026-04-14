# SNMP — Comprehensive Architecture Report

> Generated from full codebase audit of all Go source files, test files, YAML configs, and documentation.

---

## Table of Contents

1. [Module & Dependencies](#1-module--dependencies)
2. [Package Map](#2-package-map)
3. [Interfaces Defined](#3-interfaces-defined)
4. [Data Models & Structs](#4-data-models--structs)
5. [Pipeline Architecture](#5-pipeline-architecture)
6. [Pipeline Implementation](#6-pipeline-implementation)
7. [Configuration Model](#7-configuration-model)
8. [Component Deep Dive](#8-component-deep-dive)
9. [Plugin System](#9-plugin-system)
10. [Test Coverage](#10-test-coverage)
11. [Implemented vs. Planned (Architecture Spec Comparison)](#11-implemented-vs-planned)
12. [File-by-File Inventory](#12-file-by-file-inventory)

---

## 1. Module & Dependencies

**Module:** `snmp/snmp-collector`  
**Go version:** 1.25.7

| Dependency | Version | Purpose |
|---|---|---|
| `github.com/IBM/sarama` | v1.47.0 | Kafka producer client (batching, TLS, SASL) |
| `github.com/gosnmp/gosnmp` | v1.43.2 | SNMP protocol client (Get/Walk/BulkWalk, v1/v2c/v3) |
| `github.com/xdg-go/scram` | v1.2.0 | SCRAM-SHA-256/512 authentication for Kafka SASL |
| `gopkg.in/yaml.v3` | v3.0.1 | YAML configuration parsing |

The project has **minimal framework dependencies** — only gosnmp for SNMP protocol, sarama for Kafka, scram for Kafka SASL auth, and yaml.v3 for config. All pipeline logic, concurrency, connection pooling, scheduling, and formatting use the Go standard library.

---

## 2. Package Map

```
snmp/snmp-collector
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
├── format/json/                # Stage 5a: SNMPMetric → custom JSON bytes
│   └── formatter.go            #   Formatter interface, JSONFormatter
├── format/otel/                # Stage 5b: SNMPMetric → OTLP JSON bytes
│   └── formatter.go            #   OTel Formatter (ExportMetricsServiceRequest)
├── plugin/                     # Plugin system interfaces
│   ├── envelope.go             #   Envelope message type
│   ├── input.go                #   Input interface
│   ├── transport.go            #   Transport interface
│   └── transport/
│       ├── file/               #   File transport with rotation
│       │   ├── file.go
│       │   └── rotate.go
│       └── kafka/              #   Kafka transport with batching, TLS, SASL
│           ├── kafka.go
│           └── scram.go
└── pkg/snmpcollector/          # Direct pipeline implementation
    ├── app/app.go              #   Full pipeline orchestrator
    ├── config/
    │   ├── collector_config.go #   Collector process YAML schema (output enabled flags)
    │   ├── device.go           #   DeviceConfig, V3Credentials, DeviceGroup
    │   └── loader.go           #   YAML config loading from 5 directory trees
    ├── health/
    │   └── server.go           #   HTTP /health endpoint
    ├── poller/
    │   ├── poller.go           #   Poller interface, SNMPPoller (Get/Walk/BulkWalk)
    │   ├── pool.go             #   ConnectionPool — per-device SNMP session pool
    │   ├── session.go          #   gosnmp session factory (auth/priv protocol mapping)
    │   ├── session_test.go     #   Protocol alias mapping tests
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
| `Envelope` | `plugin` | Input → Transport (wraps `*SNMPMetric` with source and timestamp) |

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
                    │                          snmp Pipeline                          │
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
                    │                                                │ JSON / OTLP     │
                    │                                                ▼                  │
                    │                                           [formattedCh]          │
                    │                                                │                  │
                    │                                           [6] Transport          │
                    │                                                │ stdout/file/    │
                    │                                                │ kafka           │
                    └────────────────────────────────────────────────┼─────────────────┘
                                                                     ▼
                                                        JSON-lines or OTLP JSON output
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

## 6. Pipeline Implementation

The production binary uses a single direct pipeline implementation in `pkg/snmpcollector/app/app.go`.

### Direct Pipeline (`pkg/snmpcollector/app/app.go`)

Wires all stages directly with 4 buffered channels (`rawCh`, `decodedCh`, `metricCh`, `formattedCh`). Uses an inline `writerTransport` (writes to `os.Stdout`) when no external transport is configured. Supports `Reload()` for hot config refresh. Metrics flow directly from Producer to Formatter (no aggregator stage).

```go
type Config struct {
    ConfigPaths          config.Paths
    CollectorID          string
    PollerWorkers        int             // default 500
    BufferSize           int             // default 10000
    DecodeWorkers        int             // default 1
    ProduceWorkers       int             // default 1
    PoolOptions          poller.PoolOptions
    EnumEnabled          bool
    CounterDeltaEnabled  bool
    CounterPurgeInterval time.Duration
    PrettyPrint          bool            // custom JSON only; ignored when OTelFormat=true
    OTelFormat           bool            // emit OTLP JSON instead of custom JSON
    OTelScopeName        string          // OTLP scope name (default "snmp-collector")
    OTelScopeVersion     string          // OTLP scope version string
    Transport            plugin.Transport // nil = inline stdout writer
    ConfigReloadInterval time.Duration
}
```

**Stage goroutines launched by `Start()`:**

| Stage | Goroutines | Input | Output |
|---|---|---|---|
| Decode | `DecodeWorkers` (default 1) | `rawCh` | `decodedCh` |
| Produce | `ProduceWorkers` (default 1) | `decodedCh` | `metricCh` |
| Format | 1 | `metricCh` | `formattedCh` |
| Transport | 1 | `formattedCh` | `Transport.Send()` |
| Config Reload | 0 or 1 | ticker | `App.Reload()` |
| Counter Purge | 0 or 1 | ticker | `CounterState.Purge()` |

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
    Devices      string  // env: INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH
    DeviceGroups string  // env: INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH
    ObjectGroups string  // env: INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH
    Objects      string  // env: INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH
    Enums        string  // env: PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH
}
```

Defaults to `/etc/snmp_collector/snmp/{devices,device_groups,object_groups,objects,enums}`.

### Collector Process Config (`config/collector_config.go`)

Top-level YAML schema for the collector process itself (log, pipeline tuning, output transport selection):

```go
type CollectorConfig struct {
    Log struct {
        Level  string // debug|info|warn|error
        Format string // json|text
    }
    CollectorID string
    Pipeline struct {
        BufferSize     int
        DecodeWorkers  int
        ProduceWorkers int
    }
    Poller struct {
        Workers int
        Pool struct {
            MaxIdle     int
            IdleTimeout string // e.g. "30s"
        }
    }
    Processors struct {
        Enum    struct{ Enable bool }
        Counter struct{ DeltaEnable bool; PurgeInterval string }
    }
    Format struct {
        Pretty           bool
        OTel             bool   // emit OTLP JSON instead of custom JSON
        OTelScopeName    string // default "snmp-collector"
        OTelScopeVersion string
    }
    ConfigPaths struct {
        Devices, DeviceGroups, ObjectGroups, Objects, Enums string
    }
    ConfigReloadInterval string
    Health struct{ Addr string } // e.g. ":8080"; empty disables
    Output struct {
        Stdout CollectorStdoutConfig // enabled bool
        File   CollectorFileConfig   // enabled, path, max_bytes, max_backups
        Kafka  CollectorKafkaConfig  // enabled, brokers, topic, tls, sasl, ...
    }
}
```

**Output selection rule:** exactly one block must have `enabled: true`. The binary enforces this at startup and returns an error if more than one is enabled.

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

**Device** (`config/devices/localhost.yml`):
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

**Object Definition** (`config/objects/system.yml`):
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
```

---

## 8. Component Deep Dive

### 8.1 Entry Point (`cmd/snmpcollector/main.go`, 439 lines)

The `run()` function uses a three-level config precedence model:

```
Priority (highest → lowest): CLI flag > YAML file > hardcoded default
```

**Steps:**

1. **Pre-scan** `os.Args` for `-config` before `flag.Parse()` (YAML loaded first so values become flag defaults)
2. **Register all flags** with YAML-derived defaults
3. **`flag.Parse()`** — CLI flags override YAML values
4. **Select output transport** (see logic below)
5. **Build and start** `app.App` with merged config
6. **Optional health server** on `-health.addr` (default `:9080`)
7. **Block** on `SIGINT`/`SIGTERM` then cascade shutdown

**Output transport selection logic:**

```
With config file (-config flag present):
  Count outputs with enabled: true
  If count > 1 → error: "exactly one output block must have enabled: true"
  kafka.enabled  → build Kafka transport (IBM/sarama)
  file.enabled   → build File transport (RotatingFile)
  default        → stdout (nil Transport, inline writerTransport)

CLI-only mode (no -config flag):
  kafka brokers non-empty → build Kafka transport
  file path non-empty     → build File transport
  default                 → stdout
```

### 8.2 Scheduler (`pkg/.../scheduler/`, 303 lines)

**Timer algorithm**: Sort-to-next approach:
1. Sort entries by `nextRun` ascending (min-heap)
2. Sleep until earliest entry's `nextRun`
3. On wake, dispatch all entries where `nextRun ≤ now`
4. Use `TrySubmit()` (non-blocking) — full queue causes warning log, no blocking
5. Advance each fired entry by its `interval`

**Hot reload**: `Reload(newCfg)` — rebuilds entry heap atomically under mutex. Added devices get `nextRun = now`.

### 8.3 Poller (`pkg/.../poller/`, 710 lines total)

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

**SNMPv3 auth/priv protocol aliases** (all case-insensitive):

| Config string | Maps to |
|---|---|
| `md5` | HMAC-MD5 |
| `sha`, `sha1`, `sha128` | HMAC-SHA-1 |
| `sha224` / `sha256` / `sha384` / `sha512` | HMAC-SHA-224/256/384/512 |
| `des`, `des56` | DES-56 |
| `aes`, `aes128` | AES-128 |
| `aes192` / `aes256` | AES-192 / AES-256 |
| `aes192c` / `aes256c` | AES-192C / AES-256C (Cisco) |

**Worker Pool** (`WorkerPool`):
- N goroutines (default 500) pulling from a buffered jobs channel (capacity = `numWorkers × 2`)
- Failed polls with no varbinds are dropped before the output channel — prevents flooding downstream with empty error records
- `Submit()` blocking, `TrySubmit()` non-blocking

### 8.4 Decoder (`snmp/decoder/`, 776 lines total)

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

### 8.5 Producer (`producer/metrics/`, 660 lines total)

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

### 8.6 Formatters

Two formatters share the same interface (`format/json.Formatter`):

```go
type Formatter interface {
    Format(*SNMPMetric) ([]byte, error)
}
```

**JSON Formatter** (`format/json/`):
- Stateless, concurrent-safe
- Compact (production) and pretty-print (debug) modes via `Config.PrettyPrint`
- Uses standard `json.Marshal` / `json.MarshalIndent`

**OTel Formatter** (`format/otel/`):
- Outputs an OTLP `ExportMetricsServiceRequest` JSON payload (OpenTelemetry wire format)
- No external SDK — schema implemented with stdlib types
- Mapping: `Device` → `resource.attributes`; one `SNMPMetric` → one `ResourceMetrics` → one `ScopeMetrics`
- `Counter32`/`Counter64` → `sum` (isMonotonic=true, CUMULATIVE); all other numeric types → `gauge`
- String/byte values skipped (not representable as OTLP numbers)
- Multiple metrics with the same name are merged into one OTLP metric with multiple data points
- UCUM unit strings derived from SNMP syntax (`BandwidthMBits` → `Mbit/s`, `TimeTicks` → `cs`, etc.)
- Enabled via `format.otel: true` in collector config or `-format.otel` CLI flag

### 8.7 Transports

**Stdout** (inline `writerTransport` in `app.go`):
- Active when `cfg.Transport == nil`
- Writes to `os.Stdout`; no separate plugin package

**File** (`plugin/transport/file/`, 324 lines):
- Wraps `RotatingFile` — size-based file rotation
- Rotation scheme: `metrics.json` → `.1` → `.2` → ... (configurable max backups)
- Thread-safe with mutex
- Config: `FilePath`, `MaxBytes` (default 50 MB), `MaxBackups` (default 5)

**Kafka** (`plugin/transport/kafka/`, 567 lines):
- Uses `github.com/IBM/sarama` sync producer
- Background worker goroutine batches messages; flushes on `MaxEvents` or `FlushInterval`
- TLS support: CA cert, client cert/key, `InsecureSkipVerify`
- SASL support: PLAIN, SCRAM-SHA-256, SCRAM-SHA-512 (via `xdg-go/scram`)
- Non-blocking `Send()` — drops message if internal channel full
- Config: `Brokers`, `Topic`, `MaxEvents` (default 1000), `FlushInterval` (default 5s), `BufferSize` (default 10000), `RequiredAcks`, `Compression`, `Version`

### 8.8 Health Server (`pkg/snmpcollector/health/server.go`, 68 lines)

- HTTP server exposing `GET /health`
- Returns `200 OK` with JSON body `{"status":"ok","collector_id":"...","uptime_s":...}`
- Enabled via `-health.addr :9080` (or `health.addr` in YAML)
- Graceful shutdown via `Stop(ctx)`

---

## 9. Plugin System

### Architecture

The plugin system defines the transport interface in `plugin/`:

```
Internal pipeline ──formats──▶ Transport plugin
```

**Envelope** — Standard internal message:

```go
type Envelope struct {
    Source    string           // producer identifier
    Timestamp time.Time        // when data was collected
    Metric   *models.SNMPMetric
}
```

### Existing Plugin Implementations

| Plugin | Type | Package | Description |
|---|---|---|---|
| File | Transport | `plugin/transport/file` | Rotating file output |
| Kafka | Transport | `plugin/transport/kafka` | Kafka producer with batching, TLS, SASL |

### Plugin Development

Per `docs/plugin-dev-guide.md`, new plugins:
1. Create a package under `plugin/transport/<name>/`
2. Implement the `plugin.Transport` interface
3. Use compile-time check: `var _ plugin.Transport = (*Transport)(nil)`
4. Accept `*slog.Logger` and `Config` struct
5. Ensure concurrent `Send()` safety and graceful `Close()` lifecycle

---

## 10. Test Coverage

### Test Files

| File | Tests | Coverage Area |
|---|---|---|
| `format/json/formatter_test.go` | 15+ tests | Construction, nil input, JSON schema compliance (top-level keys, RFC3339 timestamps, device fields, metrics array structure, value types, tags), metadata, compact vs pretty, edge cases |
| `format/otel/formatter_test.go` | 18 tests | Nil input, resource attributes, scope name/version, Counter64→sum, Counter32→sum, Gauge32→gauge, int64/float64/string values, multiple instances, multiple metric names, data point attributes, timeUnixNano, unit mapping, empty metrics, device tags |
| `snmp/decoder/decoder_test.go` | 10+ tests | VarbindParser (count, instance extraction, tag flagging, empty def error), SNMPDecoder (happy path, empty varbinds), ConvertValue (BandwidthMBits, Counter64, MACAddress, IpAddress, error types) |
| `producer/metrics/producer_test.go` | 20+ tests | EnumRegistry (integer, bitmap, OID, passthrough), CounterState (first/second observation, Counter32 wrap, purge), Build (metric count, tags, override resolution Counter64>Counter32, enum resolution, counter delta, metadata, empty varbinds), MetricsProducer end-to-end |
| `pkg/snmpcollector/config/loader_test.go` | — | Config loading, path resolution |
| `pkg/snmpcollector/poller/session_test.go` | 30 tests | Auth protocol aliases (md5, sha1, sha128, sha256…), priv protocol aliases (des56, aes128, aes192…), case-insensitive matching |
| `pkg/snmpcollector/poller/poller_test.go` | 16 tests | Connection pool, session lifecycle, poll operations, dial errors, worker pool error/cancel/backpressure |
| `pkg/snmpcollector/scheduler/scheduler_test.go` | 16 tests | Timer algorithm, hot reload, concurrent reload, TrySubmit backpressure |
| `pkg/snmpcollector/app/app_test.go` | — | End-to-end pipeline |

All core tests use shared IF-MIB::ifEntry fixtures for consistency.

---

## 11. Implemented vs. Planned (Architecture Spec Comparison)

Comparing `SNMP-Architecture.md` (the original spec) against actual code:

### Fully Implemented

| Component | Spec | Code |
|---|---|---|
| SNMP Poller | ✅ Scheduler, Get/Walk/BulkWalk, connection pool | `pkg/snmpcollector/poller/`, `scheduler/` |
| SNMP Decoder | ✅ PDU parsing, type conversion, varbind matching | `snmp/decoder/` |
| Producer | ✅ Normalize, enrich (enum), counter delta, override resolution | `producer/metrics/` |
| JSON Formatter | ✅ Compact + pretty, full schema | `format/json/` |
| **OTel Formatter** | ✅ OTLP JSON (ExportMetricsServiceRequest), resource/scope/metric mapping, unit strings | `format/otel/` |
| Stdout Transport | ✅ Inline writer in app.go | `pkg/snmpcollector/app/app.go` |
| File Transport | ✅ With size-based rotation | `plugin/transport/file/` |
| **Kafka Transport** | ✅ Batching, TLS, SASL, compression | `plugin/transport/kafka/` |
| Plugin Architecture | ✅ Transport interface | `plugin/` |
| YAML Config Hierarchy | ✅ 5-directory model with env var paths | `pkg/snmpcollector/config/` |
| Collector Process Config | ✅ YAML with output `enabled` flags | `pkg/snmpcollector/config/collector_config.go` |
| SNMPv1/v2c/v3 | ✅ Including USM auth/priv | `pkg/snmpcollector/poller/session.go` |
| Hot Reload | ✅ `Scheduler.Reload()` and `App.Reload()` | Both |
| Graceful Shutdown | ✅ Signal handling, cascading channel close | `cmd/snmpcollector/main.go`, `app.go` |
| Connection Pool | ✅ Per-device, LIFO, idle timeout, concurrency limit | `pkg/snmpcollector/poller/pool.go` |
| Health Endpoint | ✅ HTTP `/health` with uptime and collector ID | `pkg/snmpcollector/health/server.go` |

### Not Implemented (Planned in Spec)

| Component | Spec Description | Status |
|---|---|---|
| **Trap Listener** | UDP port 162, v1/v2c/v3 trap parsing, inform ACK | **Not implemented** — no `snmp/trap/`, no `pkg/snmpcollector/trapreceiver/` |
| **MIB Parser** | ASN.1 MIB file parser, OID tree, resolver | **Not implemented** — uses hand-authored YAML object definitions instead |
| **MIB Tool** | `cmd/mibtool/` CLI utility | **Not implemented** |
| **Protobuf Formatter** | Binary serialization format | **Not implemented** |
| **OpenMetrics/Prometheus Formatter** | Prometheus exposition format | **Not implemented** |
| **Time Series Producer** | `producer/timeseries/` (Prometheus, InfluxDB, OpenTSDB) | **Not implemented** |
| **Event Producer** | `producer/event/` (trap→alert conversion, severity mapping) | **Not implemented** |
| **Aggregate Producer** | `producer/metrics/aggregate.go` | **Not implemented** |
| **Credentials Manager** | `pkg/snmpcollector/credentials/` — key derivation, rotation | **Not implemented** |
| **Custom SNMP Client** | `snmp/client/` with v1/v2c/v3 implementations | **Not implemented** — uses `gosnmp` directly |
| **Rate Limiting** | Per-device SNMP rate limiting | **Not implemented** (concurrency limited via connection pool semaphore) |
| **Duplicate Trap Detection** | Trap dedup | **Not implemented** |

### Summary

The **polling pipeline is fully implemented and production-ready**: Config → Scheduler → Poller → Decoder → Producer → Formatter → Transport (stdout / file / Kafka). Two output formats are supported: custom JSON and OpenTelemetry OTLP JSON. The MIB tooling layer is also unimplemented — the system relies on hand-authored YAML object definitions instead of dynamic MIB parsing.

---

## 12. File-by-File Inventory

### Go Source Files (31 files)

| File | Package | Lines | Key Exports |
|---|---|---|---|
| `cmd/snmpcollector/main.go` | `main` | 439 | `main()`, `run()`, `preScanFlag()` |
| `models/config.go` | `models` | 83 | `ObjectDefinition`, `IndexDefinition`, `AttributeDefinition`, `OverrideReference` |
| `models/metric.go` | `models` | 52 | `SNMPMetric`, `Device`, `Metric`, `MetricMetadata` |
| `plugin/envelope.go` | `plugin` | 47 | `Envelope`, `Valid()` |
| `plugin/transport.go` | `plugin` | 40 | `Transport` interface |
| `plugin/transport/file/file.go` | `file` | 117 | `Transport` (implements `plugin.Transport`), `Config`, `New()` |
| `plugin/transport/file/rotate.go` | `file` | 207 | `RotatingFile`, `RotateConfig`, `NewRotatingFile()` |
| `plugin/transport/kafka/kafka.go` | `kafka` | 510 | `Transport` (implements `plugin.Transport`), `Config`, `TLSConfig`, `SASLConfig`, `New()`, `Send()`, `Close()` |
| `plugin/transport/kafka/scram.go` | `kafka` | 57 | SCRAM-SHA-256/512 sarama auth handler |
| `producer/metrics/producer.go` | `metrics` | 121 | `Producer` interface, `MetricsProducer`, `Config`, `New()`, `Produce()` |
| `producer/metrics/poll.go` | `metrics` | 216 | `Build()`, `BuildOptions` |
| `producer/metrics/normalize.go` | `metrics` | 174 | `CounterState`, `CounterKey`, `DeltaResult`, `Delta()`, `IsCounterSyntax()`, `WrapForSyntax()` |
| `producer/metrics/enrich.go` | `metrics` | 149 | `EnumRegistry`, `IntEnum`, `NewEnumRegistry()`, `RegisterIntEnum()`, `RegisterOIDEnum()`, `Resolve()` |
| `snmp/decoder/decoder.go` | `decoder` | 174 | `Decoder` interface, `SNMPDecoder`, `RawPollResult`, `DecodedPollResult`, `NewSNMPDecoder()`, `Decode()` |
| `snmp/decoder/varbind.go` | `decoder` | 176 | `DecodedVarbind`, `VarbindParser`, `NewVarbindParser()`, `Parse()` |
| `snmp/decoder/types.go` | `decoder` | 426 | `PDUTypeString()`, `IsErrorType()`, `ConvertValue()` |
| `format/json/formatter.go` | `json` | 120 | `Formatter` interface, `JSONFormatter`, `Config`, `New()`, `Format()` |
| `format/otel/formatter.go` | `otel` | ~290 | `Formatter`, `Config`, `New()`, `Format()`, OTLP JSON schema types |
| `pkg/snmpcollector/app/app.go` | `app` | ~545 | `App`, `Config` (incl. OTelFormat/OTelScopeName/OTelScopeVersion), `New()`, `Start()`, `Stop()`, `Reload()` |
| `pkg/snmpcollector/config/collector_config.go` | `config` | ~290 | `CollectorConfig` (incl. Format.OTel/OTelScopeName/OTelScopeVersion), `DefaultCollectorConfig()`, `LoadCollectorConfig()` |
| `pkg/snmpcollector/config/device.go` | `config` | 85 | `DeviceConfig`, `V3Credentials`, `DeviceGroup`, `ObjectGroup` |
| `pkg/snmpcollector/config/loader.go` | `config` | 615 | `Paths`, `PathsFromEnv()`, `LoadedConfig`, `Load()` |
| `pkg/snmpcollector/health/server.go` | `health` | 68 | `Server`, `NewServer()`, `Start()`, `Stop()` |
| `pkg/snmpcollector/poller/poller.go` | `poller` | 227 | `Poller` interface, `PollJob`, `SNMPPoller`, `Poll()`, `LowestCommonOID()` |
| `pkg/snmpcollector/poller/pool.go` | `poller` | 249 | `ConnectionPool`, `PoolOptions`, `NewConnectionPool()`, `Get()`, `Put()`, `Discard()`, `Close()` |
| `pkg/snmpcollector/poller/session.go` | `poller` | 128 | `NewSession()`, `mapAuthProto()`, `mapPrivProto()`, `snmpv3MsgFlags()` |
| `pkg/snmpcollector/poller/worker.go` | `poller` | 110 | `WorkerPool`, `NewWorkerPool()`, `Start()`, `Submit()`, `TrySubmit()`, `Stop()` |
| `pkg/snmpcollector/scheduler/scheduler.go` | `scheduler` | 215 | `Scheduler`, `JobSubmitter` interface, `New()`, `Start()`, `Stop()`, `Reload()`, `Entries()` |
| `pkg/snmpcollector/scheduler/resolve.go` | `scheduler` | 88 | `ResolveJobs()` |
| `internal/noop/noop.go` | `noop` | 9 | `Writer{}` — no-op io.Writer for test loggers |

**Total: ~6,050 lines of Go source code** (excluding tests)

### Import Dependency Graph

```
models ← (no imports)
  ↑
  ├── snmp/decoder ← models, gosnmp
  │     ↑
  │     ├── producer/metrics ← models, snmp/decoder
  │     │     ↑
  │     │     ├── format/json ← models
  │     │     └── format/otel ← models, internal/noop
  │     │
  │     └── pkg/snmpcollector/app ← config, poller, scheduler, decoder, metrics, format/json, format/otel, plugin
  │
  ├── plugin ← models
  │     ├── plugin/transport/file ← plugin
  │     └── plugin/transport/kafka ← plugin, sarama, scram
  │
  ├── pkg/snmpcollector/config ← models, yaml.v3
  │     ↑
  │     ├── pkg/snmpcollector/poller ← models, config, decoder, gosnmp
  │     │     ↑
  │     │     └── pkg/snmpcollector/scheduler ← config, poller
  │     │
  │     └── cmd/snmpcollector/main ← app, config, poller, file-transport, kafka-transport, health
  │
  └── pkg/snmpcollector/health ← (stdlib only)
```

### Output Formats

Two output formats are available, selected by `format.otel` in the collector config:

**Custom JSON (default)** — JSON-lines, one document per line:
```json
{"timestamp":"2026-04-04T15:53:46.514+07:00","device":{"hostname":"myswitch.lab","ip_address":"127.0.0.1","snmp_version":"2c"},"metrics":[{"oid":"1.3.6.1.2.1.11.4.0","name":"snmp.msgs.community_unknown.in","instance":"0","value":0,"type":"Counter32","syntax":"Counter32"}],"metadata":{"collector_id":"dev-collector-01","poll_duration_ms":0,"poll_status":"success"}}
```

**OTLP JSON** (`format.otel: true`) — one `ExportMetricsServiceRequest` per poll result:
```json
{"resourceMetrics":[{"resource":{"attributes":[{"key":"host.name","value":{"stringValue":"myswitch.lab"}},{"key":"net.host.ip","value":{"stringValue":"127.0.0.1"}},{"key":"snmp.version","value":{"stringValue":"2c"}}]},"scopeMetrics":[{"scope":{"name":"snmp-collector","version":"1.0.0"},"metrics":[{"name":"snmp.msgs.community_unknown.in","sum":{"dataPoints":[{"attributes":[{"key":"instance","value":{"stringValue":"0"}}],"timeUnixNano":"1743767626514000000","asInt":"0"}],"aggregationTemporality":2,"isMonotonic":true}}]}]}]}
```

---

## Summary

**snmp-collector** is a production-quality SNMP polling collector with:
- A clean 6-stage pipeline (Scheduler → Poller → Decoder → Producer → Formatter → Transport)
- **Two output formats**: custom JSON schema and OpenTelemetry OTLP JSON (`format.otel: true`)
- **Three output transports**: stdout, rotating file, and Kafka (with TLS + SASL)
- `enabled: true/false` output selection — exactly one transport active per run, validated at startup
- Comprehensive SNMP v1/v2c/v3 support with full auth/priv protocol aliases (`sha1`, `sha128`, `des56`, `aes128`, etc.)
- Rich type conversion (50+ SNMP syntax types)
- Enum resolution (integer/bitmap/OID), counter delta computation with wrap detection
- Config hot-reload, graceful cascade shutdown, and an HTTP health endpoint

