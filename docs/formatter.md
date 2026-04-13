# Formatters — Stage 5

## Overview

Stage 5 of the pipeline serialises a `models.SNMPMetric` (from Stage 4 —
`producer/metrics`) into a `[]byte` payload that is forwarded to the transport.

```
producer/metrics [Stage 4]
     │  models.SNMPMetric
     ▼
Formatter [Stage 5]          ← format/json  OR  format/otel
     │  []byte
     ▼
plugin/transport/* [Stage 6]
```

Two formatters are available. The active one is selected by the `format.otel`
config flag (see [configuration.md](configuration.md)):

| Formatter | Package | Output |
|---|---|---|
| **JSON** (default) | `format/json` | Custom JSON schema |
| **OTLP JSON** | `format/otel` | OpenTelemetry `ExportMetricsServiceRequest` |

---

## Formatter interface

Both formatters implement the same interface defined in `format/json`:

```go
type Formatter interface {
    Format(metric *models.SNMPMetric) ([]byte, error)
}
```

Returns a non-nil error only on `metric == nil` or a marshalling failure. The
returned byte slice is always non-nil on success.

---

## JSON Formatter (`format/json`)

### Config

```go
type Config struct {
    PrettyPrint bool   // default false — compact output for production
    Indent      string // indent string when PrettyPrint=true; default "  "
}
```

### Output schema

```json
{
  "timestamp": "2026-02-26T10:30:00.123Z",
  "device": {
    "hostname": "router01.example.com",
    "ip_address": "192.168.1.1",
    "snmp_version": "2c",
    "vendor": "Cisco",
    "tags": { "site": "dc1" }
  },
  "metrics": [
    {
      "oid": "1.3.6.1.2.1.2.2.1.10.1",
      "name": "netif.bytes.in",
      "instance": "1",
      "value": 1234567890,
      "type": "Counter64",
      "syntax": "Counter64",
      "tags": { "netif.descr": "GigabitEthernet0/0/1" }
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
| Timestamp format | RFC 3339 Nano — Go's `time.Time` default |
| `value` type | Preserved as-is: `uint64`/`int64`/`float64` → JSON number, `string` → JSON string |
| `tags` omitted | When `nil` or empty |
| `instance` omitted | When `""` |
| `metadata` omitted | When zero value |

### YAML config

```yaml
format:
  pretty: false   # true = indented JSON (useful for debugging)
```

---

## OTel Formatter (`format/otel`)

Outputs an **OTLP `ExportMetricsServiceRequest`** — the standard
OpenTelemetry wire format for metrics, compatible with any OTLP receiver
(Grafana Alloy, OpenTelemetry Collector, etc.).

No external OpenTelemetry SDK is required; the schema is implemented with
standard library types.

### Config

```go
type Config struct {
    ScopeName    string // default "snmp-collector"
    ScopeVersion string // optional
}
```

### SNMPMetric → OTLP mapping

| `models.SNMPMetric` field | OTLP location |
|---|---|
| `Device.Hostname` | `resource.attributes["host.name"]` |
| `Device.IPAddress` | `resource.attributes["net.host.ip"]` |
| `Device.SNMPVersion` | `resource.attributes["snmp.version"]` |
| `Device.Vendor` | `resource.attributes["device.vendor"]` (omitted when empty) |
| `Device.Model` | `resource.attributes["device.model"]` (omitted when empty) |
| `Device.SysDescr` | `resource.attributes["device.sys_descr"]` (omitted when empty) |
| `Device.SysLocation` | `resource.attributes["device.sys_location"]` (omitted when empty) |
| `Device.Tags["k"]` | `resource.attributes["device.tag.k"]` |
| `Timestamp` | `dataPoints[].timeUnixNano` (decimal string, nanoseconds) |
| `Metric.Name` | `metrics[].name` — same name → one OTLP metric, multiple data points |
| `Metric.Instance` | `dataPoints[].attributes["instance"]` |
| `Metric.Tags["k"]` | `dataPoints[].attributes["k"]` |
| `Metric.Value` (int64/uint64) | `dataPoints[].asInt` (decimal string) |
| `Metric.Value` (float64) | `dataPoints[].asDouble` (JSON number) |
| `Metric.Value` (string/[]byte) | **skipped** — not representable as an OTLP number |

### SNMP type → OTLP instrument type

| SNMP type | OTLP instrument | `isMonotonic` | Temporality |
|---|---|---|---|
| `Counter32`, `Counter64` | `sum` | `true` | `CUMULATIVE` (2) |
| All other numeric types | `gauge` | — | — |

### Syntax → OTLP unit

| Config syntax | OTLP unit (UCUM) |
|---|---|
| `BandwidthBits` | `bit/s` |
| `BandwidthKBits` | `kbit/s` |
| `BandwidthMBits` | `Mbit/s` |
| `BandwidthGBits` | `Gbit/s` |
| `TimeTicks` | `cs` (centiseconds) |
| All others | `""` (empty) |

### Output schema

```json
{
  "resourceMetrics": [{
    "resource": {
      "attributes": [
        {"key": "host.name",    "value": {"stringValue": "router01.example.com"}},
        {"key": "net.host.ip",  "value": {"stringValue": "192.168.1.1"}},
        {"key": "snmp.version", "value": {"stringValue": "2c"}},
        {"key": "device.vendor","value": {"stringValue": "Cisco"}}
      ]
    },
    "scopeMetrics": [{
      "scope": {"name": "snmp-collector", "version": "1.0.0"},
      "metrics": [
        {
          "name": "netif.bytes.in",
          "sum": {
            "dataPoints": [
              {
                "attributes": [
                  {"key": "instance",    "value": {"stringValue": "1"}},
                  {"key": "netif.descr", "value": {"stringValue": "Gi0/0/1"}}
                ],
                "timeUnixNano": "1740563400123000000",
                "asInt": "1234567890"
              }
            ],
            "aggregationTemporality": 2,
            "isMonotonic": true
          }
        },
        {
          "name": "cpu.util",
          "gauge": {
            "dataPoints": [
              {
                "timeUnixNano": "1740563400123000000",
                "asInt": "42"
              }
            ]
          }
        }
      ]
    }]
  }]
}
```

### YAML config

```yaml
format:
  otel: true
  otel_scope_name: "snmp-collector"    # optional, this is the default
  otel_scope_version: "1.0.0"          # optional
```

### CLI flags

```
-format.otel                    enable OTLP JSON output
-format.otel.scope-name         override instrumentation scope name
-format.otel.scope-version      set instrumentation scope version
```

---

## Concurrency

Both formatters are **stateless** after construction — all fields are immutable.
`Format` is safe to call from any number of concurrent goroutines without
additional synchronisation.
