# SNMP Decoder Module

## Overview

The decoder is **Stage 3 of the processing pipeline** — it sits between the poller and the producer.

```
Poller  →  [rawData channel]  →  Decoder  →  [decodedData channel]  →  Producer
```

Its job is narrow and well-defined:

> Given a raw gosnmp PDU response and the `ObjectDefinition` that drove the request, produce a flat slice of native-typed, named `DecodedVarbind` values with table instances extracted.

It does **not** group by instance, assemble tag maps, normalise timestamps, or calculate counter deltas — those are Producer responsibilities.

---

## Package Layout

```
snmp/decoder/
├── decoder.go    # Decoder interface, SNMPDecoder implementation, channel message types
├── varbind.go    # VarbindParser — OID matching, instance extraction
├── types.go      # SNMP type system: PDUTypeString, IsErrorType, ConvertValue
└── decoder_test.go
```

```
models/
├── metric.go     # SNMPMetric, Device, Metric, MetricMetadata
└── config.go     # ObjectDefinition, AttributeDefinition, IndexDefinition, OverrideReference
```

---

## Data Flow

```
RawPollResult
  ├── Device              → forwarded verbatim to DecodedPollResult
  ├── ObjectDef           → drives attribute OID lookup in VarbindParser
  ├── Varbinds []SnmpPDU  → decoded one-by-one
  ├── CollectedAt         → forwarded; used to compute PollDurationMs
  └── PollStartedAt       → used to compute PollDurationMs

        ↓  SNMPDecoder.Decode()

DecodedPollResult
  ├── Device              (unchanged)
  ├── ObjectDefKey        (e.g. "IF-MIB::ifEntry")
  ├── CollectedAt         (unchanged)
  ├── PollDurationMs      (CollectedAt − PollStartedAt in ms)
  └── Varbinds []DecodedVarbind
        ├── OID            ("1.3.6.1.2.1.2.2.1.10.1")
        ├── AttributeName  ("netif.bytes.in")
        ├── Instance       ("1")
        ├── Value          (uint64(1234567890))
        ├── SNMPType       ("Counter32")
        ├── Syntax         ("Counter32")
        └── IsTag          (false)
```

---

## Types

### `RawPollResult` ← input from Poller

Defined in `snmp/decoder/decoder.go`. Placed on the raw-data channel by the poller worker after a successful SNMP Get/GetBulk/Walk.

| Field | Type | Description |
|---|---|---|
| `Device` | `models.Device` | Identifying context: hostname, IP, SNMP version |
| `ObjectDef` | `models.ObjectDefinition` | Configuration definition that drove this poll |
| `Varbinds` | `[]gosnmp.SnmpPDU` | Raw PDUs exactly as returned by gosnmp |
| `CollectedAt` | `time.Time` | Wall-clock time the response was received |
| `PollStartedAt` | `time.Time` | Wall-clock time the request was sent |

### `DecodedPollResult` → output to Producer

Defined in `snmp/decoder/decoder.go`. Placed on the decoded-data channel by the decoder worker.

| Field | Type | Description |
|---|---|---|
| `Device` | `models.Device` | Forwarded unchanged |
| `ObjectDefKey` | `string` | e.g. `"IF-MIB::ifEntry"` |
| `Varbinds` | `[]DecodedVarbind` | Fully decoded, typed, named variable bindings |
| `CollectedAt` | `time.Time` | Forwarded unchanged |
| `PollDurationMs` | `int64` | Round-trip latency in milliseconds |

### `DecodedVarbind`

Defined in `snmp/decoder/varbind.go`. Represents one resolved SNMP variable binding after OID matching, instance extraction, and value conversion.

| Field | Type | Description |
|---|---|---|
| `OID` | `string` | Normalised numeric OID, no leading dot |
| `AttributeName` | `string` | Config name, e.g. `"netif.bytes.in"` |
| `Instance` | `string` | Table row index suffix, e.g. `"1"`, `"192.168.1.1.5"` |
| `Value` | `interface{}` | Native Go type: `int64`, `uint64`, `float64`, `string`, `[]byte` |
| `SNMPType` | `string` | PDU type string, e.g. `"Counter32"` |
| `Syntax` | `string` | Config syntax verbatim, e.g. `"BandwidthMBits"` |
| `IsTag` | `bool` | When `true`, the Producer stores this in `Metric.Tags`, not `Metric.Value` |

---

## Configuration Types (`models/config.go`)

These types are the decoded form of the YAML object definition files under `INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH`.

### `ObjectDefinition`

Maps to one object YAML file, e.g. `IF-MIB_ifEntry.yml`.

| Field | Type | Description |
|---|---|---|
| `Key` | `string` | e.g. `"IF-MIB::ifEntry"` |
| `MIB` | `string` | MIB module, e.g. `"IF-MIB"` |
| `Object` | `string` | Object name, e.g. `"ifEntry"` |
| `Augments` | `string` | Key of the object this augments (shares its index) |
| `Index` | `[]IndexDefinition` | Table index components in order; empty for scalars |
| `DiscoveryAttribute` | `string` | Attribute used for row discovery, e.g. `"ifDescr"` |
| `Attributes` | `map[string]AttributeDefinition` | SNMP attribute name → definition |

### `AttributeDefinition`

Maps to one item under `attributes:` in the object YAML.

| Field | Type | Description |
|---|---|---|
| `OID` | `string` | Numeric OID, e.g. `".1.3.6.1.2.1.2.2.1.10"` |
| `Name` | `string` | Output metric name, e.g. `"netif.bytes.in"` |
| `Syntax` | `string` | Conversion hint, e.g. `"Counter64"`, `"BandwidthMBits"` |
| `IsTag` | `bool` | Whether this is a dimension label (`tag: true` in YAML) |
| `Overrides` | `*OverrideReference` | The 32-bit attribute this supersedes (optional) |
| `Rediscover` | `string` | `""`, `"OnChange"`, or `"OnReset"` |

---

## `Decoder` Interface

```go
type Decoder interface {
    Decode(raw RawPollResult) (DecodedPollResult, error)
}
```

Implementations **must be safe for concurrent use** — the pipeline can run multiple
decoder goroutines calling the same instance (`pipeline.decode_workers`, default `1`).

The production implementation is `SNMPDecoder`. Use the interface in all types that depend on decoding to keep them testable.

---

## `SNMPDecoder`

### Construction

```go
logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelInfo,
}))
dec := decoder.NewSNMPDecoder(logger)
```

Pass `nil` for the logger in tests — a no-op writer is substituted automatically.

### Behaviour

1. Returns an empty `DecodedPollResult` (no error) when `Varbinds` is empty.
2. Returns an error (and an empty result) when the `ObjectDef` has no attributes.
3. Returns a **partial result alongside the error** when a value conversion fails mid-batch. The caller decides whether the partial data is usable.

### Logging

All log entries use structured `slog` fields and match the `-log.fmt=json` CLI flag:

| Level | Event |
|---|---|
| `DEBUG` | Successful decode — includes `pdu_count`, `decoded_count`, `poll_duration_ms` |
| `WARN` | Empty varbind list, or all PDUs matched no configured attributes |
| `ERROR` | Partial parse failure — includes counts of decoded vs total PDUs |

---

## `VarbindParser`

### Construction

```go
parser, err := decoder.NewVarbindParser(objectDef)
```

`NewVarbindParser` pre-builds an `attrByOID` map keyed by normalised (no-leading-dot) attribute OIDs. Construction is O(a) where a = number of attributes. This is done once per `Decode` call; the parser itself is not cached because it's cheap to build and the decoder is stateless.

### OID Matching

For each incoming PDU OID, `matchAttribute` performs:

1. **Direct lookup** — tries the full OID verbatim. Matches scalars (e.g. `sysUpTime.0`).
2. **Right-to-left prefix scan** — strips one OID arc at a time from the right until a prefix matches a configured attribute OID. The stripped suffix becomes the `Instance`.

**Example:**

```
PDU OID:       1.3.6.1.2.1.2.2.1.10.1
Attribute OID: 1.3.6.1.2.1.2.2.1.10      → match
Instance:      1
```

**Compound index example (OSPF):**

```
PDU OID:       1.3.6.1.2.1.14.17.1.6.192.168.1.1.3.5.192.168.1.2.192.168.1.3
Attribute OID: 1.3.6.1.2.1.14.17.1.6     → match
Instance:      192.168.1.1.3.5.192.168.1.2.192.168.1.3
```

### Skipped PDUs

The following PDUs are silently dropped and do **not** produce errors:

- `NoSuchObject` — OID not implemented by device
- `NoSuchInstance` — OID exists but requested instance absent
- `EndOfMibView` — normal sentinel returned by Walk/GetBulk
- `Null` — empty response
- Any OID with no matching attribute prefix — normal GetBulk over-fetch

---

## Type Conversion (`ConvertValue`)

Located in `snmp/decoder/types.go`. Called for every matched PDU.

```go
func ConvertValue(rawType gosnmp.Asn1BER, rawValue interface{}, syntax string) (interface{}, error)
```

The `syntax` string is the `AttributeDefinition.Syntax` value from config. It controls **both** which Go type is returned and any unit-scale normalisation applied.

### Conversion Table

| Syntax | Go type returned | Notes |
|---|---|---|
| `Integer`, `Integer32`, `EnumInteger`, `EnumBitmap`, `TruthValue`, `RowStatus` | `int64` | |
| `Counter32`, `Counter64`, `Gauge32`, `Unsigned32`, `TimeTicks` | `uint64` | |
| `DisplayString`, `OctetString`, `DateAndTime` | `string` | Trailing null bytes stripped |
| `PhysAddress`, `MacAddress` | `string` | Colon-separated hex, e.g. `"00:1a:2b:3c:4d:5e"` |
| `ObjectIdentifier`, `EnumObjectIdentifier` | `string` | No leading dot |
| `IpAddress`, `IpAddressNoSuffix` | `string` | Dotted decimal, e.g. `"192.0.2.1"` |
| `BandwidthBits` | `float64` | Raw value (already bits/sec) |
| `BandwidthKBits` | `float64` | ×1,000 → bits/sec |
| `BandwidthMBits` | `float64` | ×1,000,000 → bits/sec |
| `BandwidthGBits` | `float64` | ×1,000,000,000 → bits/sec |
| `BytesB` | `uint64` | Raw bytes |
| `BytesKB` / `BytesMB` / `BytesGB` / `BytesTB` | `float64` | ×1,000 / ×1e6 / ×1e9 / ×1e12 |
| `BytesKiB` / `BytesMiB` / `BytesGiB` | `float64` | ×1,024 / ×1,048,576 / ×1,073,741,824 |
| `TemperatureC` | `float64` | Raw (already Celsius) |
| `TemperatureDeciC` | `float64` | ÷10 → Celsius |
| `TemperatureCentiC` | `float64` | ÷100 → Celsius |
| `PowerWatt` | `float64` | Raw (already Watts) |
| `PowerMilliWatt` | `float64` | ÷1,000 → Watts |
| `PowerKiloWatt` | `float64` | ×1,000 → Watts |
| `CurrentAmp` / `CurrentMilliAmp` / `CurrentMicroAmp` | `float64` | ÷1 / ÷1,000 / ÷1,000,000 → Amps |
| `VoltageVolt` / `VoltageMilliVolt` / `VoltageMicroVolt` | `float64` | ÷1 / ÷1,000 / ÷1,000,000 → Volts |
| `FreqHz` / `FreqKHz` / `FreqMHz` / `FreqGHz` | `float64` | ×1 / ×1,000 / ×1e6 / ×1e9 → Hz |
| `TicksSec` / `TicksMilliSec` / `TicksMicroSec` | `uint64` | Raw ticks; unit normalization is CLI-controlled |
| `Percent1` / `Percent100` / `PercentDeci100` | `float64` | Raw value; downstream respects `PROCESSOR_PERCENT_NORM` |
| Unknown / future syntax | best-effort | Falls back to PDU-type-based conversion; no error |

> **Enum resolution is not performed here.** `EnumInteger`, `EnumBitmap`, and `EnumObjectIdentifier` return the **raw numeric value**. The Producer stage — which holds the enum registry — handles the integer → string translation at its layer.

### Error Conditions

`ConvertValue` returns an error only when:
- The PDU type is an error sentinel (`NoSuchObject`, `NoSuchInstance`, `EndOfMibView`, `Null`)
- The raw value cannot be coerced to the required Go type (e.g. a negative integer into `uint64`)

Unknown syntax falls back silently rather than erroring, so future syntax additions in config don't break older collector binaries.

---

## Error Handling

The decoder follows the pipeline's **partial-result principle**: a conversion failure on one varbind does not discard the successfully decoded varbinds that came before it in the same PDU batch.

```
RawPollResult (13 PDUs)
  ├── 11 decode ok  →  DecodedPollResult.Varbinds (len=11)
  ├──  1 NoSuchInstance  →  silently skipped (no error)
  └──  1 conversion error  →  error returned, partial result usable
```

The caller (pipeline supervisor or decoder worker loop) should:
- **Log** the error with the `device` and `object` fields.
- **Emit the partial result** to the producer channel so the 11 good metrics are not lost.
- **Increment** the `poll_errors_total{device,error_type="decode"}` Prometheus counter.

The production app worker loop follows this rule: on decode error it forwards
the partial result when `len(result.Varbinds) > 0`, and only drops the record
when no decoded varbinds are available. There is no aggregator stage downstream —
partial results flow directly to the producer.

---

## Usage Example

### In a pipeline worker

```go
dec := decoder.NewSNMPDecoder(logger)

// rawCh is populated by the poller worker pool
for raw := range rawCh {
    result, err := dec.Decode(raw)
    if err != nil {
        metrics.PollErrorsTotal.WithLabelValues(raw.Device.Hostname, "decode").Inc()
        logger.Error("decoder error", "device", raw.Device.Hostname, "err", err)
        // fall through — result may still contain partial varbinds
    }
    if len(result.Varbinds) > 0 {
        decodedCh <- result
    }
}
```

### In a unit test

```go
dec := decoder.NewSNMPDecoder(nil) // nil → no-op logger

raw := decoder.RawPollResult{
    Device:        models.Device{Hostname: "sw01", IPAddress: "192.0.2.1", SNMPVersion: "2c"},
    ObjectDef:     ifEntryDef,       // your models.ObjectDefinition
    Varbinds:      pdus,             // []gosnmp.SnmpPDU from your mock
    CollectedAt:   time.Now(),
    PollStartedAt: time.Now().Add(-50 * time.Millisecond),
}

result, err := dec.Decode(raw)
// assert err == nil, len(result.Varbinds) == expectedCount, etc.
```

---

## What the Decoder Does NOT Do

| Concern | Handled by |
|---|---|
| Counter delta (rate) calculation | Producer — `normalize.go` |
| Grouping varbinds by table instance | Producer — `poll.go` |
| Building `Metric.Tags` from tag varbinds | Producer — `enrich.go` |
| Enum integer → string resolution | Producer — uses enum registry |
| Augmentation merging (`augments:` field) | Producer |
| `overrides:` resolution (64-bit over 32-bit) | Producer |
| JSON serialisation | Formatter — `format/json/` |
| File / stdout delivery | Transport — `plugin/transport/file/`, `plugin/transport/stdout/` |
