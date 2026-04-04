# snmp Architecture

> **Module:** `github.com/kazuyuki114/snmp`  
> **Go version:** 1.25.7  
> **Dependencies:** `gosnmp v1.43.2`, `gopkg.in/yaml.v3 v3.0.1`

---

## Table of Contents

1. [Overview](#1-overview)
2. [High-Level Architecture](#2-high-level-architecture)
3. [Directory Structure](#3-directory-structure)
4. [Two Pipeline Modes](#4-two-pipeline-modes)
5. [Configuration Model](#5-configuration-model)
6. [Pipeline Stages](#6-pipeline-stages)
7. [Plugin System](#7-plugin-system)
8. [Data Models](#8-data-models)
9. [Syntax Types Reference](#9-syntax-types-reference)
10. [JSON Output Format](#10-json-output-format)
11. [Command-Line Reference](#11-command-line-reference)
12. [Interfaces Reference](#12-interfaces-reference)
13. [Graceful Shutdown](#13-graceful-shutdown)
14. [Configuration File Examples](#14-configuration-file-examples)

---

## 1. Overview

snmp is a high-performance, channel-based SNMP polling collector written in Go.
It periodically polls network devices via SNMP (v1, v2c, v3), decodes the raw PDU
responses into structured metrics, and writes the results as NDJSON (newline-
delimited JSON) to **stdout** or a **rotating file**.

**Key characteristics:**

- **Poll-only** — no SNMP trap receiver.
- **No MIB compiler** — object definitions are pre-compiled into YAML files.
- **Two external dependencies** — `gosnmp` for SNMP I/O, `yaml.v3` for config parsing.
- **Six-stage pipeline** — Scheduler → WorkerPool → Decoder → Producer → Formatter → Transport.
- **Plugin architecture** — Telegraf-style Input / Transport interfaces for extensibility.
- **YAML-driven configuration** — five directory trees control devices, groups, objects and enums.

---

## 2. High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        cmd/snmpcollector                        │
│                         (entry point)                           │
└──────────────────────────────┬──────────────────────────────────┘
                               │
                      ┌────────▼────────┐
                      │   config.Load   │  ← reads 5 YAML directories
                      └────────┬────────┘
                               │
          ┌────────────────────▼─────────────────────┐
          │              app.App  (direct pipeline)  │
          │                                           │
          │  ┌───────────┐     ┌──────────┐          │
          │  │ Scheduler │────▶│WorkerPool│ (N goroutines)
          │  └───────────┘     └────┬─────┘          │
          │                         │ rawCh          │
          │                    ┌────▼─────┐          │
          │                    │ Decoder  │          │
          │                    └────┬─────┘          │
          │                         │ decodedCh      │
          │                    ┌────▼─────┐          │
          │                    │ Producer │          │
          │                    └────┬─────┘          │
          │                         │ metricCh       │
          │                    ┌────▼──────┐         │
          │                    │ Formatter │  (JSON)  │
          │                    └────┬──────┘         │
          │                         │ formattedCh    │
          │                    ┌────▼──────┐         │
          │                    │ Transport │  (stdout / file)
          │                    └───────────┘         │
          └──────────────────────────────────────────┘
```

All inter-stage communication uses buffered Go channels. The default buffer
size is **10 000** per channel.

---

## 3. Directory Structure

```
snmp/
├── cmd/snmpcollector/
│   └── main.go                 # Entry point, CLI flags, signal handling
├── models/
│   ├── config.go               # ObjectDefinition, IndexDefinition, AttributeDefinition
│   └── metric.go               # SNMPMetric, Device, Metric, MetricMetadata
├── pkg/snmpcollector/
│   ├── app/
│   │   └── app.go              # Direct pipeline orchestrator (App)
│   ├── config/
│   │   ├── device.go           # DeviceConfig, V3Credentials, DeviceGroup, ObjectGroup
│   │   └── loader.go           # YAML loader — Load(), PathsFromEnv()
│   ├── poller/
│   │   ├── poller.go           # SNMPPoller, PollJob, Get/Walk/BulkWalk
│   │   ├── pool.go             # ConnectionPool with per-device concurrency
│   │   ├── session.go          # NewSession — DeviceConfig → *gosnmp.GoSNMP
│   │   └── worker.go           # WorkerPool — fan-out dispatcher
│   └── scheduler/
│       ├── scheduler.go        # Timer-based job dispatcher
│       └── resolve.go          # ResolveJobs — config hierarchy → PollJob list
├── snmp/decoder/
│   ├── decoder.go              # SNMPDecoder, RawPollResult, DecodedPollResult
│   ├── varbind.go              # VarbindParser — OID matching & value extraction
│   └── types.go                # ConvertValue — 50+ syntax type conversions
├── producer/metrics/
│   ├── producer.go             # MetricsProducer, Producer interface
│   ├── poll.go                 # Build() — 6-step metric assembly
│   ├── normalize.go            # CounterState — delta calculation for Counter32/64
│   └── enrich.go               # EnumRegistry — integer/bitmap/OID enum resolution
├── format/json/
│   └── formatter.go            # JSONFormatter, Formatter interface
├── plugin/
│   ├── envelope.go             # Envelope struct
│   ├── input.go                # Input interface
│   ├── transport.go            # Transport interface
│   ├── engine/
│   │   └── engine.go           # Plugin Engine broker (Telegraf-style)
│   ├── input/snmppoller/
│   │   └── snmppoller.go       # SNMP Poller as Input plugin
│   └── transport/
│       ├── file/
│       │   ├── file.go         # File Transport plugin
│       │   └── rotate.go       # RotatingFile — size-based file rotation
│       └── stdout/
│           └── stdout.go       # Stdout Transport plugin
└── testdata/
    ├── devices/                # Device YAML files
    ├── device_groups/          # Device group YAML files
    ├── object_groups/          # Object group YAML files
    ├── objects/                # Object definition YAML files
    └── enums/                  # Enum definition YAML files
```

---

## 4. Two Pipeline Modes

### 4.1 Direct Pipeline (`app.App`)

Used by `cmd/snmpcollector/main.go`. The `App` struct owns all six pipeline
stages and wires them together with four Go channels:

```
rawCh → decodedCh → metricCh → formattedCh
```

Output is written via an internal `writerTransport` that writes to an
`io.Writer` (defaults to `os.Stdout`; can be a `RotatingFile`).

This is the **production mode**.

### 4.2 Plugin Engine (`plugin/engine.Engine`)

A Telegraf-inspired broker that manages N Input plugins and M Transport plugins:

```
Input₁ ─┐                            ┌─▶ Transport₁
Input₂ ─┤──▶ [envelopeCh] ──▶ Fmt ──┤──▶ Transport₂
InputN ─┘                            └─▶ TransportN
```

- All Inputs share a single `envelopeCh` of `plugin.Envelope`.
- The engine runs configurable `FormatWorkers` (default 50) goroutines.
- Each format worker serialises the Envelope's `*models.SNMPMetric` to JSON
  and fans the resulting `[]byte` to every Transport.

**Implemented plugins:**

| Type | Name | Package |
|------|------|---------|
| Input | `snmp_poller` | `plugin/input/snmppoller` |
| Transport | `stdout` | `plugin/transport/stdout` |
| Transport | `file` | `plugin/transport/file` |

---

## 5. Configuration Model

### 5.1 Five-Directory Hierarchy

Configuration is loaded from five directory trees. Each directory is set via
an environment variable or a CLI flag override:

| Directory | Environment Variable | CLI Flag | Default Path |
|-----------|---------------------|----------|-------------|
| Devices | `INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH` | `--config.devices` | `/etc/snmp_collector/snmp/devices` |
| Device Groups | `INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH` | `--config.device.groups` | `/etc/snmp_collector/snmp/device_groups` |
| Object Groups | `INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH` | `--config.object.groups` | `/etc/snmp_collector/snmp/object_groups` |
| Objects | `INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH` | `--config.objects` | `/etc/snmp_collector/snmp/objects` |
| Enums | `PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH` | `--config.enums` | `/etc/snmp_collector/snmp/enums` |

### 5.2 Resolution Chain

```
Device → DeviceGroups[] → ObjectGroups[] → Objects[] → ObjectDefinitions
```

1. Each **device** YAML file lists one or more `device_groups`.
2. Each **device group** lists one or more `object_groups`.
3. Each **object group** lists one or more object keys (e.g. `"IF-MIB::ifEntry"`).
4. Each **object** key resolves to an `ObjectDefinition` with `index` and `attributes`.

Objects that appear via multiple groups are **deduplicated** per device.

### 5.3 Hard-Coded Fallback Defaults

When a device YAML field is zero-valued, these defaults are applied:

| Field | Default |
|-------|---------|
| `port` | `161` |
| `poll_interval` | `60` (seconds) |
| `timeout` | `3000` (milliseconds) |
| `retries` | `2` |
| `version` | `"2c"` |
| `max_concurrent_polls` | `4` |

### 5.4 LoadedConfig

The `config.Load()` function reads all five directories and returns a
`LoadedConfig`:

```go
type LoadedConfig struct {
    Devices      map[string]DeviceConfig            // hostname → device config
    DeviceGroups map[string]DeviceGroup              // group name → DeviceGroup
    ObjectGroups map[string]ObjectGroup              // group name → ObjectGroup
    ObjectDefs   map[string]models.ObjectDefinition  // object key → definition
    Enums        *metrics.EnumRegistry               // enum translations (may be nil)
}
```

Only object definitions referenced by the device→group→object chain are loaded,
keeping memory usage proportional to the active inventory.

---

## 6. Pipeline Stages

### Stage 1 — Scheduler

**Package:** `pkg/snmpcollector/scheduler`

The scheduler maintains one timer per device. It resolves the configuration
hierarchy into `PollJob` values via `ResolveJobs()`, sorts entries by
`nextRun`, and fires jobs using `TrySubmit` (non-blocking). If the worker pool
is full, the job is dropped with a warning.

Key behaviours:
- Polls all devices immediately on start.
- Supports hot `Reload()` — new devices start immediately; removed devices stop.
- Uses `time.NewTimer` for precise interval dispatch.

### Stage 2 — Worker Pool

**Package:** `pkg/snmpcollector/poller`

`WorkerPool` runs N goroutines (default 500) that dequeue `PollJob` values from
an internal channel and execute them via a `Poller` interface. Results
(`decoder.RawPollResult`) are sent to `rawCh`.

**SNMP operation selection** (`SNMPPoller.Poll`):

| Object Type | SNMP Version | Operation |
|-------------|-------------|-----------|
| Scalar (no index) | any | `Get` (all attribute OIDs + `.0`) |
| Table | v1 | `WalkAll` (lowest common OID) |
| Table | v2c / v3 | `BulkWalkAll` (lowest common OID) |

**Connection pool** (`ConnectionPool`):
- Per-device concurrency semaphore (default 4 slots).
- LIFO idle stack with configurable max idle sessions and idle timeout.
- Sessions are created via `NewSession`, which maps `DeviceConfig` to `*gosnmp.GoSNMP`.

**SNMPv3 support:**
- Security levels: `AuthPriv`, `AuthNoPriv`, `NoAuthNoPriv`.
- Auth protocols: `MD5`, `SHA`, `SHA224`, `SHA256`, `SHA384`, `SHA512`.
- Privacy protocols: `DES`, `AES`, `AES192`, `AES256`, `AES192C`, `AES256C`.

### Stage 3 — Decoder

**Package:** `snmp/decoder`

The `SNMPDecoder` converts each `RawPollResult` into a `DecodedPollResult`:

1. Constructs a `VarbindParser` from the `ObjectDefinition`.
2. For each `gosnmp.SnmpPDU`:
   - Skips error sentinels (`NoSuchObject`, `NoSuchInstance`, `EndOfMibView`, `Null`).
   - Matches the OID against attribute definitions by longest-prefix lookup.
   - Extracts the table row instance suffix.
   - Converts the raw value using `ConvertValue()` and the configured syntax type.
3. Returns a `DecodedPollResult` with a flat slice of `DecodedVarbind` values.

The decoder is **stateless** and safe for concurrent use.

### Stage 4 — Producer

**Package:** `producer/metrics`

The `MetricsProducer` converts `DecodedPollResult` into `models.SNMPMetric`
via the `Build()` function. Six steps:

1. **Partition** — Separate tag varbinds from metric varbinds.
2. **Group** — Group both sets by table instance.
3. **Override resolution** — Per instance+name, keep the highest-priority
   syntax (e.g. `Counter64` beats `Counter32`).
4. **Enum resolution** — If enabled, translate `EnumInteger`, `EnumBitmap`,
   and `EnumObjectIdentifier` values to text labels via `EnumRegistry`.
5. **Counter delta** — If enabled, replace raw `Counter32`/`Counter64` values
   with per-interval deltas. Counter32 wraps at 2³² − 1; Counter64 at 2⁶⁴ − 1.
   First observation emits 0.
6. **Assemble** — Build `models.Metric` for each non-tag varbind with its
   instance's tags attached.

**EnumRegistry:**
- Integer enums: `int64` value → label string.
- Bitmap enums: comma-joined labels of set bits.
- OID enums: OID string → label string.
- Safe fallback: missing enums return the raw value unchanged.

**CounterState:**
- Thread-safe (`sync.Mutex`).
- Keyed by `(Device, Attribute, Instance)`.
- `Purge()` method for cleaning stale entries.

### Stage 5 — Formatter

**Package:** `format/json`

The `JSONFormatter` serialises `models.SNMPMetric` using `encoding/json`.
Supports optional `PrettyPrint` mode with configurable indent string.

This is the **only** implemented formatter. The `Formatter` interface exists
for future formats (Protobuf, Prometheus, InfluxDB).

### Stage 6 — Transport

**Direct mode (`app.App`):** Uses an internal `writerTransport` that writes
each formatted `[]byte` + newline to an `io.Writer`. Defaults to `os.Stdout`;
when `--output.file` is set, wraps a `RotatingFile`.

**Plugin mode:** Stdout and File Transport plugins implement `plugin.Transport`.

**File rotation** (`plugin/transport/file/rotate.go`):
- Rotates when active file exceeds `MaxBytes`.
- Keeps up to `MaxBackups` rotated files.
- Atomic rename: `metrics.json` → `metrics.json.1` (numbered backup).

---

## 7. Plugin System

### 7.1 Core Abstractions

```go
// plugin/envelope.go
type Envelope struct {
    Source    string             // Input plugin name
    Timestamp time.Time
    Metric   *models.SNMPMetric // nil = drop
}

// plugin/input.go
type Input interface {
    Name() string
    Start(ctx context.Context, out chan<- Envelope) error
    Stop()
}

// plugin/transport.go
type Transport interface {
    Name() string
    Send(data []byte) error
    Close() error
}
```

### 7.2 Engine Lifecycle

```go
eng := engine.New(engine.Config{
    BufferSize:    10_000,
    FormatWorkers: 50,
    PrettyPrint:   false,
}, logger)

eng.AddInput(snmpPoller)
eng.AddTransport(stdoutTransport)
eng.AddTransport(fileTransport)

eng.Start(ctx)   // non-blocking
// ...
eng.Stop()        // graceful drain
```

**Shutdown order:**
1. Cancel context → Inputs begin draining.
2. `Input.Stop()` called for all inputs; once all return, `envelopeCh` is closed.
3. Format workers drain the channel and exit.
4. `Transport.Close()` called for all transports.

### 7.3 SNMP Poller Input Plugin

`plugin/input/snmppoller` wraps the core pipeline stages (Scheduler →
WorkerPool → Decoder → Producer) into a single `plugin.Input`. It owns its
own `rawCh` and `decodedCh` and produces `Envelope` values on the shared
output channel.

This plugin supports `Reload()` for hot configuration changes.

---

## 8. Data Models

### 8.1 Configuration Types (`models/config.go`)

```go
type ObjectDefinition struct {
    Key                string                          // "IF-MIB::ifEntry"
    MIB                string                          // "IF-MIB"
    Object             string                          // "ifEntry"
    Augments           string                          // references parent table index
    Index              []IndexDefinition               // table index components
    DiscoveryAttribute string                          // for table row detection
    Attributes         map[string]AttributeDefinition  // name → attribute
}

type IndexDefinition struct {
    Type   string  // "Integer", "IpAddress", "OctetString"
    OID    string  // ".1.3.6.1.2.1.2.2.1.1"
    Name   string  // "netif"
    Syntax string  // display/conversion hint
}

type AttributeDefinition struct {
    OID        string              // ".1.3.6.1.2.1.2.2.1.10"
    Name       string              // "netif.bytes.in"
    Syntax     string              // "Counter64", "BandwidthMBits", etc.
    IsTag      bool                // dimension label, not metric value
    Overrides  *OverrideReference  // higher-precision replacement
    Rediscover string              // "", "OnChange", "OnReset"
}

type OverrideReference struct {
    Object    string  // "IF-MIB::ifEntry"
    Attribute string  // "ifInOctets"
}
```

### 8.2 Device Configuration (`pkg/snmpcollector/config/device.go`)

```go
type DeviceConfig struct {
    IP                 string
    Port               int             // default 161
    PollInterval       int             // seconds, default 60
    Timeout            int             // ms, default 3000
    Retries            int             // default 2
    ExponentialTimeout bool
    Version            string          // "1", "2c", "3"
    Communities        []string        // v1/v2c
    V3Credentials      []V3Credentials // v3
    DeviceGroups       []string
    MaxConcurrentPolls int             // default 4
}

type V3Credentials struct {
    Username                 string
    AuthenticationProtocol   string  // noauth, md5, sha, sha224..sha512
    AuthenticationPassphrase string
    PrivacyProtocol          string  // nopriv, des, aes, aes192..aes256c
    PrivacyPassphrase        string
}
```

### 8.3 Output Types (`models/metric.go`)

```go
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
    CollectorID    string `json:"collector_id"`
    PollDurationMs int64  `json:"poll_duration_ms"`
    PollStatus     string `json:"poll_status"`  // "success" | "timeout" | "error"
}
```

### 8.4 Pipeline Message Types

```go
// snmp/decoder — input to decoder
type RawPollResult struct {
    Device       models.Device
    ObjectDef    models.ObjectDefinition
    Varbinds     []gosnmp.SnmpPDU
    CollectedAt  time.Time
    PollStartedAt time.Time
}

// snmp/decoder — output of decoder
type DecodedPollResult struct {
    Device         models.Device
    ObjectDefKey   string
    Varbinds       []DecodedVarbind
    CollectedAt    time.Time
    PollDurationMs int64
}

type DecodedVarbind struct {
    OID           string
    AttributeName string       // "netif.bytes.in"
    Instance      string       // "1" for table row, "" for scalar
    Value         interface{}  // int64 | uint64 | float64 | string | []byte
    SNMPType      string       // "Counter64", "Integer", etc.
    Syntax        string       // config syntax verbatim
    IsTag         bool
}

// poller — unit of work
type PollJob struct {
    Hostname     string
    Device       models.Device
    DeviceConfig config.DeviceConfig
    ObjectDef    models.ObjectDefinition
}
```

---

## 9. Syntax Types Reference

The `ConvertValue()` function in `snmp/decoder/types.go` handles the following
syntax types. All conversions produce `int64`, `uint64`, `float64`, `string`,
or `[]byte`.

### Integer Types → `int64`

| Syntax | Description |
|--------|-------------|
| `Integer` | Standard SNMP Integer |
| `Integer32` | Synonym for Integer |
| `InterfaceIndex` | Interface index value |
| `InterfaceIndexOrZero` | Interface index or zero |
| `TruthValue` | Boolean as integer (1=true, 2=false) |
| `RowStatus` | SNMP RowStatus enumeration |
| `TimeStamp` | Timestamp value |
| `TimeInterval` | Time interval value |
| `EnumInteger` | Integer with enum resolution |
| `EnumIntegerKeepID` | Enum-resolved, keeps numeric ID |
| `EnumBitmap` | Bitmap with per-bit enum labels |

### Unsigned / Counter Types → `uint64`

| Syntax | Description |
|--------|-------------|
| `Unsigned32` | Unsigned 32-bit integer |
| `Gauge32` | Non-wrapping gauge |
| `Counter32` | 32-bit monotonic counter |
| `Counter64` | 64-bit monotonic counter |
| `TimeTicks` | Hundredths of a second |
| `Opaque` | Raw opaque value |

### String Types → `string`

| Syntax | Description |
|--------|-------------|
| `DisplayString` | Human-readable text (null-trimmed) |
| `OctetString` | Byte string as UTF-8 |
| `DateAndTime` | SNMP DateAndTime textual convention |
| `PhysAddress` | Colon-separated hex (e.g. `00:1a:2b:3c:4d:5e`) |
| `MacAddress` | Same as PhysAddress |
| `ObjectIdentifier` | Dotted-decimal OID string |
| `EnumObjectIdentifier` | OID with enum resolution |
| `EnumObjectIdentifierKeepOID` | Enum-resolved, keeps OID string |
| `IpAddress` | Dotted-decimal IP (e.g. `192.168.1.1`) |
| `IpAddressNoSuffix` | Same as IpAddress |

### Bandwidth Types → `float64` (normalised to bits/sec)

| Syntax | Multiplier |
|--------|-----------|
| `BandwidthBits` | ×1 |
| `BandwidthKBits` | ×1,000 |
| `BandwidthMBits` | ×1,000,000 |
| `BandwidthGBits` | ×1,000,000,000 |

### Bytes Types → `float64` (normalised to bytes)

| Syntax | Multiplier |
|--------|-----------|
| `BytesB` | ×1 (returns `uint64`) |
| `BytesKB` | ×1,000 |
| `BytesMB` | ×1,000,000 |
| `BytesGB` | ×1,000,000,000 |
| `BytesTB` | ×1,000,000,000,000 |
| `BytesKiB` | ×1,024 |
| `BytesMiB` | ×1,048,576 |
| `BytesGiB` | ×1,073,741,824 |

### Temperature Types → `float64` (normalised to °C)

| Syntax | Conversion |
|--------|-----------|
| `TemperatureC` | raw value (°C) |
| `TemperatureDeciC` | ÷10 |
| `TemperatureCentiC` | ÷100 |

### Power Types → `float64` (normalised to Watts)

| Syntax | Conversion |
|--------|-----------|
| `PowerWatt` | raw value |
| `PowerMilliWatt` | ÷1,000 |
| `PowerKiloWatt` | ×1,000 |

### Current Types → `float64` (normalised to Amps)

| Syntax | Conversion |
|--------|-----------|
| `CurrentAmp` | raw value |
| `CurrentMilliAmp` | ÷1,000 |
| `CurrentMicroAmp` | ÷1,000,000 |

### Voltage Types → `float64` (normalised to Volts)

| Syntax | Conversion |
|--------|-----------|
| `VoltageVolt` | raw value |
| `VoltageMilliVolt` | ÷1,000 |
| `VoltageMicroVolt` | ÷1,000,000 |

### Frequency Types → `float64` (normalised to Hz)

| Syntax | Multiplier |
|--------|-----------|
| `FreqHz` | ×1 |
| `FreqKHz` | ×1,000 |
| `FreqMHz` | ×1,000,000 |
| `FreqGHz` | ×1,000,000,000 |

### Duration / Tick Types → `uint64` (raw value)

| Syntax | Unit |
|--------|------|
| `TicksSec` | seconds |
| `TicksMilliSec` | milliseconds |
| `TicksMicroSec` | microseconds |

### Percentage Types → `float64`

| Syntax | Range |
|--------|-------|
| `Percent1` | 0–1 |
| `Percent100` | 0–100 |
| `PercentDeci100` | 0–1000 (represents 0–100.0%) |

### Fallback Behaviour

Unknown syntax types trigger a best-effort conversion based on the raw SNMP
PDU type, ensuring the pipeline is never broken by unrecognised config values.

---

## 10. JSON Output Format

The formatter produces **NDJSON** — each poll result per object is a single
JSON line. Example (pretty-printed for readability):

```json
{
  "timestamp": "2025-07-08T02:26:52.397072368Z",
  "device": {
    "hostname": "localhost",
    "ip_address": "127.0.0.1",
    "snmp_version": "2c"
  },
  "metrics": [
    {
      "oid": "1.3.6.1.2.1.1.1.0",
      "name": "sys.descr",
      "instance": "0",
      "value": "Linux LENOVO 5.15.167.4-microsoft-standard-WSL2",
      "type": "OctetString",
      "syntax": "DisplayString"
    },
    {
      "oid": "1.3.6.1.2.1.1.3.0",
      "name": "sys.uptime",
      "instance": "0",
      "value": 1766717,
      "type": "TimeTicks",
      "syntax": "TimeTicks"
    },
    {
      "oid": "1.3.6.1.2.1.1.5.0",
      "name": "sys.name",
      "instance": "0",
      "value": "LENOVO",
      "type": "OctetString",
      "syntax": "DisplayString"
    }
  ],
  "metadata": {
    "collector_id": "LENOVO",
    "poll_duration_ms": 2,
    "poll_status": "success"
  }
}
```

Table metrics include per-instance tags:

```json
{
  "oid": "1.3.6.1.2.1.2.2.1.10.1",
  "name": "netif.bytes.in",
  "instance": "1",
  "value": 0,
  "type": "Counter32",
  "syntax": "Counter32",
  "tags": {
    "netif.name": "lo"
  }
}
```

---

## 11. Command-Line Reference

```
snmpcollector [flags]
```

### Logging

| Flag | Default | Description |
|------|---------|-------------|
| `--log.level` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `--log.fmt` | `json` | Log format: `json`, `text` |

### Collector

| Flag | Default | Description |
|------|---------|-------------|
| `--collector.id` | hostname | Collector instance ID |
| `--format.pretty` | `false` | Pretty-print JSON output |

### Poller / Pipeline

| Flag | Default | Description |
|------|---------|-------------|
| `--poller.workers` | `500` | Number of concurrent poller goroutines |
| `--pipeline.buffer.size` | `10000` | Inter-stage channel buffer capacity |

### Processor

| Flag | Default | Description |
|------|---------|-------------|
| `--processor.enum.enable` | `false` | Enable enum resolution |
| `--processor.counter.delta` | `true` | Enable counter delta computation |

### Connection Pool

| Flag | Default | Description |
|------|---------|-------------|
| `--snmp.pool.max.idle` | `2` | Max idle connections per device |
| `--snmp.pool.idle.timeout` | `30` | Idle connection timeout (seconds) |

### Output

| Flag | Default | Description |
|------|---------|-------------|
| `--output.file` | `""` (stdout) | Write metrics to file (enables rotation) |
| `--output.file.max-bytes` | `52428800` (50 MB) | Rotate file at this size |
| `--output.file.max-backups` | `5` | Rotated backup files to keep (0 = keep all) |

### Config Path Overrides

| Flag | Overrides Environment Variable |
|------|-------------------------------|
| `--config.devices` | `INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH` |
| `--config.device.groups` | `INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH` |
| `--config.object.groups` | `INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH` |
| `--config.objects` | `INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH` |
| `--config.enums` | `PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH` |

### Signal Handling

- **SIGINT / SIGTERM** → graceful shutdown.

---

## 12. Interfaces Reference

### Decoder

```go
// snmp/decoder
type Decoder interface {
    Decode(raw RawPollResult) (DecodedPollResult, error)
}
```

### Producer

```go
// producer/metrics
type Producer interface {
    Produce(decoded decoder.DecodedPollResult) (models.SNMPMetric, error)
}
```

### Formatter

```go
// format/json
type Formatter interface {
    Format(metric *models.SNMPMetric) ([]byte, error)
}
```

### Poller

```go
// pkg/snmpcollector/poller
type Poller interface {
    Poll(ctx context.Context, job PollJob) (decoder.RawPollResult, error)
}
```

### JobSubmitter

```go
// pkg/snmpcollector/scheduler
type JobSubmitter interface {
    Submit(poller.PollJob)
    TrySubmit(poller.PollJob) bool
}
```

### Plugin Input

```go
// plugin
type Input interface {
    Name() string
    Start(ctx context.Context, out chan<- Envelope) error
    Stop()
}
```

### Plugin Transport

```go
// plugin
type Transport interface {
    Name() string
    Send(data []byte) error
    Close() error
}
```

---

## 13. Graceful Shutdown

### Direct Pipeline (`app.App`)

1. Cancel the pipeline context (stops scheduler and worker pool sources).
2. Wait for the scheduler goroutine to exit.
3. Drain the worker pool (waits for in-flight SNMP polls to complete).
4. Close `rawCh` → decoder drains → closes `decodedCh` → producer drains →
   closes `metricCh` → formatter drains → closes `formattedCh`.
5. Transport goroutine drains `formattedCh` → exits.
6. Close transport and connection pool.

### Plugin Engine

1. Cancel context → Inputs begin draining.
2. `Input.Stop()` for each input; on completion, `envelopeCh` is closed.
3. Format workers drain the envelope channel and exit.
4. `Transport.Close()` for each transport.

---

## 14. Configuration File Examples

### Device (`testdata/devices/localhost.yml`)

```yaml
localhost:
  ip: 127.0.0.1
  port: 161
  poll_interval: 300
  timeout: 5000
  retries: 2
  version: "2c"
  communities:
    - "public"
  device_groups:
    - "generic"
```

### Device Group (`testdata/device_groups/generic.yml`)

```yaml
generic:
  object_groups:
    - ietf_system
    - ietf_host
    - ietf_netif_ethernet
    - ietf_ip
    - ietf_tcp
    - ietf_udp
    - ietf_icmp
    - ietf_snmp
    - ucdavis_cpu
    - ucdavis_diskio
    - ucdavis_memory
    - ucdavis_load
    - ucdavis_system_stats
```

### Object Group (`testdata/object_groups/system.yml`)

```yaml
ietf_system:
  objects:
    - "SNMPv2-MIB::system"
```

### Object Definition (`testdata/objects/system.yml`)

```yaml
SNMPv2-MIB::system:
  mib: SNMPv2-MIB
  object: system
  attributes:
    sysDescr:
      oid: ".1.3.6.1.2.1.1.1"
      name: sys.descr
      syntax: DisplayString
      is_tag: false
    sysUpTime:
      oid: ".1.3.6.1.2.1.1.3"
      name: sys.uptime
      syntax: TimeTicks
      is_tag: false
    sysName:
      oid: ".1.3.6.1.2.1.1.5"
      name: sys.name
      syntax: DisplayString
      is_tag: false
```
