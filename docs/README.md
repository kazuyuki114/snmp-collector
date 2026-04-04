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
./snmpcollector -config testdata/collector_config.yml
```

The sample file at `testdata/collector_config.yml` points `config_paths` at the
`testdata/` directories and outputs pretty-printed JSON to stdout — useful for
development without any extra setup.

### Run with flags only

```bash
./snmpcollector \
  -config.devices=./testdata/devices \
  -config.device.groups=./testdata/device_groups \
  -config.object.groups=./testdata/object_groups \
  -config.objects=./testdata/objects \
  -config.enums=./testdata/enums \
  -log.level=debug \
  -log.fmt=text \
  -format.pretty \
  -poller.workers=2
```

### Run with Kafka output

```bash
./snmpcollector \
  -config testdata/collector_config.yml \
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
Example `testdata/devices/localhost.yml`:

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

### Built-in transports

| Plugin | Package | Description |
|---|---|---|
| **Stdout** | `pkg/snmpcollector/app` (inline) | Writes JSON + newline to `os.Stdout`. Default when no output is configured. |
| **File** | `plugin/transport/file` | Writes JSON to a rotating file. Activated by `output.file.path`. |
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
| [formatter.md](formatter.md) | JSON formatter — `Formatter` interface, `Config`, `Format()` schema, timestamp format, value type preservation, pretty-print, concurrency contract |
| [poller.md](poller.md) | SNMP poller — `Poller` interface, `ConnectionPool`, `WorkerPool`, session factory, operation selection (Get/Walk/BulkWalk), concurrency contract |
| [scheduler.md](scheduler.md) | Polling scheduler — `Scheduler`, `JobSubmitter` interface, `ResolveJobs()` config hierarchy resolution, timer management, hot reload |
| [plugin-dev-guide.md](plugin-dev-guide.md) | Writing new Transport plugins — interfaces, package layout, step-by-step guide, design rules |

## Pipeline stages (build order)

```
[1]  models/                          — shared data contract (no deps)
[2]  snmp/decoder/                    — PDU → DecodedVarbind
[3]  producer/metrics/                — DecodedVarbind → SNMPMetric
[4]  format/json/                     — SNMPMetric → []byte (JSON)
[5]  pkg/snmpcollector/config/        — YAML config loader (SNMP data + collector config)
[6]  pkg/snmpcollector/poller/        — SNMP Get/Bulk/Walk + connection pool
[7]  pkg/snmpcollector/scheduler/     — polling job queue
[8]  pkg/snmpcollector/app/           — pipeline wiring + lifecycle
[9]  plugin/                          — Transport interface
[10] plugin/transport/file/           — File transport (rotation)
[11] plugin/transport/kafka/          — Kafka transport (batching, TLS, SASL)
[12] cmd/snmpcollector/               — binary entry point
```
