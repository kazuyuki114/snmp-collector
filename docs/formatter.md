# `format/json` — JSON Formatter

## Overview

The `format/json` package is **Stage 5** of the SNMP Collector pipeline. It serialises
a `models.SNMPMetric` value (produced by Stage 4 — `producer/metrics`) into a JSON byte
slice that is passed to the transport layer.

```
producer/metrics [Stage 4]
     │  models.SNMPMetric
     ▼
format/json.JSONFormatter [Stage 5]
     │  []byte  (JSON)
     ▼
plugin/transport/* [Stage 6]
```

JSON is the **primary and currently only implemented output format**. The package is
designed so that additional formatters (Protobuf, Prometheus, InfluxDB …) can be added
by implementing the `Formatter` interface without modifying any other package.

---

## Formatter interface

```go
type Formatter interface {
    Format(metric *models.SNMPMetric) ([]byte, error)
}
```

`JSONFormatter` implements `Formatter`. Downstream callers (e.g. the transport layer)
may program against this interface so they remain format-agnostic.

---

## Config

```go
type Config struct {
    PrettyPrint bool   // default false — compact output for production
    Indent      string // indent string when PrettyPrint=true; default "  " (two spaces)
}
```

---

## Construction

```go
func New(cfg Config, logger *slog.Logger) *JSONFormatter
```

- If `logger` is `nil`, a no-op writer is substituted; the formatter never panics.
- If `PrettyPrint=true` and `Indent=""`, the indent defaults to two spaces `"  "`.

---

## Format()

```go
func (f *JSONFormatter) Format(metric *models.SNMPMetric) ([]byte, error)
```

Returns a non-nil error only when `metric == nil` or `json.Marshal` fails. The returned
byte slice is always non-nil on success.

---

## JSON schema

The output matches the architecture spec exactly. All JSON keys come from the `json`
struct tags on the model types in `models/metric.go`.

```json
{
  "timestamp": "2026-02-26T10:30:00.123Z",
  "device": {
    "hostname": "router01.example.com",
    "ip_address": "192.168.1.1",
    "snmp_version": "2c",
    "vendor": "Cisco",
    "model": "ASR1000",
    "sys_descr": "Cisco IOS Software, ASR1000 Software",
    "sys_location": "DC1-Rack5",
    "sys_contact": "netops@example.com",
    "tags": {
      "environment": "production",
      "role": "edge-router",
      "site": "dc1"
    }
  },
  "metrics": [
    {
      "oid": "1.3.6.1.2.1.2.2.1.10.1",
      "name": "netif.bytes.in",
      "instance": "1",
      "value": 1234567890,
      "type": "Counter64",
      "syntax": "Counter64",
      "tags": {
        "netif.descr": "GigabitEthernet0/0/1",
        "netif.type": "ethernetCsmacd"
      }
    },
    {
      "oid": "1.3.6.1.2.1.2.2.1.8.1",
      "name": "netif.state.oper",
      "instance": "1",
      "value": "up",
      "type": "Integer",
      "syntax": "EnumInteger",
      "tags": {
        "netif.descr": "GigabitEthernet0/0/1"
      }
    }
  ],
  "metadata": {
    "collector_id": "collector-01",
    "poll_duration_ms": 245,
    "poll_status": "success"
  }
}
```

### Key behaviours

| Behaviour | Detail |
|---|---|
| Timestamp format | RFC 3339 Nano (e.g. `"2026-02-26T10:30:00.123Z"`) — Go's `time.Time` default |
| `value` type | Preserved as-is: `uint64` → JSON number, `int64` → JSON number, `float64` → JSON number, `string` → JSON string |
| `tags` omitted | When a `Metric.Tags` map is `nil` or empty, `"tags"` is absent from the object |
| `metadata` omitted | When `MetricMetadata` is a zero value, `"metadata"` is absent (struct tag `omitempty`) |
| `instance` omitted | When `Metric.Instance` is `""`, `"instance"` is absent (struct tag `omitempty`) |

---

## Usage

```go
import (
    fmtjson "github.com/snmp/snmp_collector/format/json"
    "log/slog"
)

formatter := fmtjson.New(fmtjson.Config{}, slog.Default())

// In the pipeline worker goroutine:
data, err := formatter.Format(&metric)
if err != nil {
    // err means metric==nil or json.Marshal failed – both are programming errors
    slog.Error("format failed", "error", err)
    continue
}
// data is a JSON byte slice ready to send to transport
```

### Pretty-print (debugging / file transport)

```go
formatter := fmtjson.New(fmtjson.Config{PrettyPrint: true}, nil)
data, _ := formatter.Format(&metric)
fmt.Println(string(data))
```

---

## Concurrency

`JSONFormatter` is **stateless** after construction — all fields are immutable. It is
safe to call `Format` from any number of concurrent goroutines without additional
synchronisation.

---

## Logging

On each successful call, `Format` emits a single `slog.Debug` entry:

```json
{"level":"DEBUG","msg":"format/json: formatted metric",
 "collector_id":"collector-01","hostname":"router01.example.com",
 "metric_count":3,"bytes":1024}
```

On error:

```json
{"level":"ERROR","msg":"format/json: marshal failed",
 "collector_id":"collector-01","object_def":"router01.example.com",
 "error":"json: unsupported type: ..."}
```

---

## Edge cases

| Scenario | Behaviour |
|---|---|
| `metric == nil` | Returns `fmt.Errorf("format/json: metric must not be nil")` |
| `metric.Metrics` empty / nil | Produces `"metrics":null` or `"metrics":[]` depending on nil vs empty slice — both are valid |
| `logger == nil` | No-op writer substituted; no panic |
| Unserialisable value in `Metric.Value` | `json.Marshal` returns an error; error is logged and returned |
