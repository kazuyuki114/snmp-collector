# SNMP Collector — Developer Documentation

## Quick Start

### Prerequisites

- Go 1.21+
- `snmpd` (optional, for local testing): `sudo apt install snmpd snmp`

### Build

```bash
go build -o snmpcollector ./cmd/snmpcollector/
```

### Run with a config file (recommended)

```bash
./snmpcollector -config config/collector_config.yml
```

The sample file at `config/collector_config.yml` points `config_paths` at the
`config/` directories and outputs pretty-printed JSON to stdout — useful for
development without any extra setup.

### Run with flags only

```bash
./snmpcollector \
  -config.devices=./config/devices \
  -config.device.groups=./config/device_groups \
  -config.object.groups=./config/object_groups \
  -config.objects=./config/objects \
  -config.enums=./config/enums \
  -log.level=debug \
  -log.fmt=text \
  -format.pretty \
  -poller.workers=2
```

### Run with Kafka output

```bash
./snmpcollector \
  -config config/collector_config.yml \
  -output.kafka.brokers broker1:9092,broker2:9092 \
  -output.kafka.topic   snmp-metrics
```

Or put it all in the config file:

```yaml
# config.yaml
output:
  kafka:
    brokers:
      - broker1:9092
      - broker2:9092
    topic: snmp-metrics
```

```bash
./snmpcollector -config config.yaml
```

### Device configuration

Each device is defined in a YAML file under the devices directory.
Example `config/devices/localhost.yml`:

```yaml
myswitch.lab:
  ip: 127.0.0.1
  port: 161
  poll_interval: 300
  timeout: 3000
  retries: 2
  version: 2c
  communities:
    - public
  device_groups:
    - generic
  max_concurrent_polls: 2
```

Optional fields fall back to hard-coded defaults: `port=161`,
`poll_interval=60`, `timeout=3000`, `retries=2`, `version=2c`,
`max_concurrent_polls=4`.

### Running tests

```bash
go test ./... -count=1              # all tests
go test ./pkg/snmpcollector/config/ -v  # single package, verbose
go test ./... -race -count=1        # with race detector
```

---

## Architecture

The collector uses a **six-stage pipeline** wired in `pkg/snmpcollector/app/`:

```
Scheduler → WorkerPool → [rawCh] → Decoder (N) → [decodedCh] →
Producer (N) → [metricCh] → Formatter → [formattedCh] → Transport
```

### Plugin interfaces (`plugin/`)

| Interface | Description |
|---|---|
| `Transport` | Output/delivery plugin: `Name()`, `Send(data []byte)`, `Close()` |

### Built-in formatters

| Formatter | Package | Description |
|---|---|---|
| **JSON** | `format/json` | Custom JSON schema (default). Compact or pretty-printed. |
| **OTLP JSON** | `format/otel` | OpenTelemetry `ExportMetricsServiceRequest` JSON. Enabled with `format.otel: true`. |

### Built-in transports

| Plugin | Package | Description |
|---|---|---|
| **Stdout** | `pkg/snmpcollector/app` (inline) | Writes formatted bytes + newline to `os.Stdout`. Default when no output is configured. |
| **File** | `plugin/transport/file` | Writes to a rotating file. Activated by `output.file.path`. |
| **Kafka** | `plugin/transport/kafka` | Batched delivery to Apache Kafka via `sarama.SyncProducer`. Activated by `output.kafka.brokers`. |

---

## Configuration

The collector accepts configuration from three sources, in priority order:

```
CLI flags  >  YAML config file  >  environment variables (paths only)  >  built-in defaults
```

See [configuration.md](configuration.md) for the full field reference and
annotated examples.

### Output transport selection

```
output.kafka.brokers non-empty?  →  Kafka transport
    else output.file.path non-empty?  →  File transport
        else  →  Stdout (default)
```

---

## Module docs

| Document | What it covers |
|---|---|
| [configuration.md](configuration.md) | YAML config file format, all fields, CLI flags table, environment variables, priority chain |
| [kafka-transport.md](kafka-transport.md) | Kafka transport architecture, batching/flush model, TLS, SASL, concurrency contract, error handling |
| [models.md](models.md) | Core data structures (`SNMPMetric`, `Device`, `Metric`), configuration types (`ObjectDefinition`, `AttributeDefinition`), design constraints |
| [decoder.md](decoder.md) | SNMP response decoder — pipeline position, `RawPollResult` / `DecodedPollResult` channel types, `VarbindParser` OID matching, `ConvertValue` syntax-to-Go-type table, error handling, usage examples |
| [producer.md](producer.md) | Metrics producer — `EnumRegistry` (integer / bitmap / OID enums), `CounterState` (delta + wrap detection), `Build()` assembly steps, `MetricsProducer` interface, concurrency contract |
| [formatter.md](formatter.md) | Formatters — `Formatter` interface; JSON formatter (custom schema, pretty-print); OTel formatter (OTLP JSON, resource/scope/metric mapping, unit table) |
| [poller.md](poller.md) | SNMP poller — `Poller` interface, `ConnectionPool`, `WorkerPool`, session factory, operation selection (Get/Walk/BulkWalk), concurrency contract |
| [scheduler.md](scheduler.md) | Polling scheduler — `Scheduler`, `JobSubmitter` interface, `ResolveJobs()` config hierarchy resolution, timer management, hot reload |
| [plugin.md](plugin.md) | Writing new Transport plugins — interfaces, package layout, step-by-step guide, design rules |

## Pipeline stages (build order)

```
[1]  models/                          — shared data contract (no deps)
[2]  snmp/decoder/                    — PDU → DecodedVarbind
[3]  producer/metrics/                — DecodedVarbind → SNMPMetric
[4]  format/json/                     — SNMPMetric → []byte (custom JSON)
[5]  format/otel/                     — SNMPMetric → []byte (OTLP JSON)
[6]  pkg/snmpcollector/config/        — YAML config loader (SNMP data + collector config)
[7]  pkg/snmpcollector/poller/        — SNMP Get/Bulk/Walk + connection pool
[8]  pkg/snmpcollector/scheduler/     — polling job queue
[9]  pkg/snmpcollector/app/           — pipeline wiring + lifecycle
[10] plugin/                          — Transport interface
[11] plugin/transport/file/           — File transport (rotation)
[12] plugin/transport/kafka/          — Kafka transport (batching, TLS, SASL)
[13] cmd/snmpcollector/               — binary entry point
```
