# Collector Configuration

The collector can be configured via a **YAML config file**, **CLI flags**, or
both. When both are provided, CLI flags always win.

---

## Table of Contents

- [Priority Chain](#priority-chain)
- [Using a Config File](#using-a-config-file)
- [Complete Field Reference](#complete-field-reference)
  - [log](#log)
  - [collector\_id](#collector_id)
  - [pipeline](#pipeline)
  - [poller](#poller)
  - [processors](#processors)
  - [format](#format)
  - [config\_reload\_interval](#config_reload_interval)
  - [config\_paths](#config_paths)
  - [health](#health)
  - [output](#output)
- [CLI Flags Reference](#cli-flags-reference)
- [Output Transport Selection](#output-transport-selection)
- [Environment Variables](#environment-variables)
- [Annotated Example File](#annotated-example-file)

---

## Priority Chain

```
CLI flag  ──highest──►
YAML file             ►  effective value
env variable          ►  (config_paths only)
built-in default ──lowest──►
```

A CLI flag set explicitly always overrides whatever is in the YAML file.
A YAML value always overrides the environment variable fallback for
`config_paths` fields. Unset YAML fields keep their built-in defaults.

---

## Using a Config File

Pass the path with `-config`:

```bash
# YAML only
./snmpcollector -config /etc/snmpcollector/config.yaml

# YAML + one override (debug logging, everything else from file)
./snmpcollector -config /etc/snmpcollector/config.yaml -log.level debug

# No file — pure flags (original behaviour, still works)
./snmpcollector -output.kafka.brokers broker1:9092 -output.kafka.topic snmp
```

The config file is loaded once at startup. Changing it while the process is
running has no effect unless `-config.reload.interval` is also set (which
reloads the **SNMP data directories** only — see [config\_paths](#config_paths)).

---

## Complete Field Reference

### log

Controls process logging. Logs are always written to **stderr**.

| Field | Type | Default | CLI flag |
|---|---|---|---|
| `log.level` | `string` | `info` | `-log.level` |
| `log.format` | `string` | `json` | `-log.fmt` |

`log.level` values: `debug`, `info`, `warn`, `error`  
`log.format` values: `json`, `text`

```yaml
log:
  level: info
  format: json
```

---

### collector_id

```yaml
collector_id: "dc1-collector-01"
```

| Type | Default | CLI flag |
|---|---|---|
| `string` | system hostname | `-collector.id` |

Identifies this collector instance in every metric's `metadata.collector_id`
field. Defaults to the system hostname when empty.

---

### pipeline

Controls the inter-stage buffered channels and the number of parallel workers
for the decode and produce stages.

| Field | Type | Default | CLI flag |
|---|---|---|---|
| `pipeline.buffer_size` | `int` | `10000` | `-pipeline.buffer.size` |
| `pipeline.decode_workers` | `int` | `1` | `-pipeline.decode.workers` |
| `pipeline.produce_workers` | `int` | `1` | `-pipeline.produce.workers` |

```yaml
pipeline:
  buffer_size: 10000
  decode_workers: 1
  produce_workers: 1
```

Increase `decode_workers` / `produce_workers` if the decode or produce stage
becomes a bottleneck (visible via dropped-job counters in the logs).

---

### poller

Controls the SNMP polling worker pool and the per-device connection pool.

| Field | Type | Default | CLI flag |
|---|---|---|---|
| `poller.workers` | `int` | `500` | `-poller.workers` |
| `poller.pool.max_idle` | `int` | `2` | `-snmp.pool.max.idle` |
| `poller.pool.idle_timeout` | `duration` | `30s` | `-snmp.pool.idle.timeout` (seconds) |

```yaml
poller:
  workers: 500
  pool:
    max_idle: 2
    idle_timeout: 30s
```

`poller.workers` limits how many SNMP sessions are active at the same time
across all devices. `pool.max_idle` limits how many idle TCP/UDP sessions are
kept open per device between polls.

---

### processors

Controls the optional value-enrichment stages.

| Field | Type | Default | CLI flag |
|---|---|---|---|
| `processors.enum.enable` | `bool` | `false` | `-processor.enum.enable` |
| `processors.counter.delta_enable` | `bool` | `true` | `-processor.counter.delta` |
| `processors.counter.purge_interval` | `duration` | `5m` | `-processor.counter.purge.interval` (seconds) |

```yaml
processors:
  enum:
    enable: false       # resolve integer OID values to human-readable labels
  counter:
    delta_enable: true  # compute per-poll-interval deltas for Counter32/64
    purge_interval: 5m  # garbage-collect stale counter entries this often
```

Set `purge_interval` to `0s` to disable automatic garbage collection of stale
counter state.

---

### format

Controls the output format for metrics sent to the transport.

| Field | Type | Default | CLI flag |
|---|---|---|---|
| `format.pretty` | `bool` | `false` | `-format.pretty` |
| `format.otel` | `bool` | `false` | `-format.otel` |
| `format.otel_scope_name` | `string` | `snmp-collector` | `-format.otel.scope-name` |
| `format.otel_scope_version` | `string` | `""` | `-format.otel.scope-version` |

```yaml
# Custom JSON (default)
format:
  pretty: false   # true = indented JSON (useful for debugging)

# OpenTelemetry OTLP JSON
format:
  otel: true
  otel_scope_name: "snmp-collector"   # instrumentation scope name
  otel_scope_version: "1.0.0"         # instrumentation scope version (optional)
```

When `otel: true`, the output is an OTLP `ExportMetricsServiceRequest` JSON
payload instead of the custom schema. `pretty` is ignored in OTel mode.
See [formatter.md](formatter.md) for the full OTLP mapping.

---

### config_reload_interval

```yaml
config_reload_interval: 0s
```

| Type | Default | CLI flag |
|---|---|---|
| `duration` | `0s` (disabled) | `-config.reload.interval` (seconds) |

When non-zero, the collector re-reads all SNMP data directories (devices,
groups, objects, enums) on this interval and hot-reloads the scheduler so that
newly added or changed devices are picked up without a restart.

`0s` or `""` disables reload.

---

### config_paths

Override the paths to the SNMP data directories. Any field left empty falls
back to its environment variable, then to the hardcoded default.

| Field | Env variable | Hardcoded default |
|---|---|---|
| `config_paths.devices` | `INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH` | `/etc/snmp_collector/snmp/devices` |
| `config_paths.device_groups` | `INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH` | `/etc/snmp_collector/snmp/device_groups` |
| `config_paths.object_groups` | `INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH` | `/etc/snmp_collector/snmp/object_groups` |
| `config_paths.objects` | `INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH` | `/etc/snmp_collector/snmp/objects` |
| `config_paths.enums` | `PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH` | `/etc/snmp_collector/snmp/enums` |

```yaml
config_paths:
  devices:       /opt/snmp/devices
  device_groups: /opt/snmp/device_groups
  object_groups: /opt/snmp/object_groups
  objects:       /opt/snmp/objects
  enums:         /opt/snmp/enums
```

Relative paths are resolved from the **working directory** when the binary is
started, not from the config file's location.

---

### health

Exposes a `/health` HTTP endpoint for liveness checks.

| Field | Type | Default | CLI flag |
|---|---|---|---|
| `health.addr` | `string` | `""` (disabled) | `-health.addr` |

```yaml
health:
  addr: ":8080"   # e.g. curl http://localhost:8080/health
```

Empty string disables the endpoint entirely.

---

### output

Exactly one output transport is active at a time.

**Selection priority:** Kafka (if `brokers` is non-empty) → File (if `path` is
non-empty) → stdout (default).

#### output.kafka

See [kafka-transport.md](kafka-transport.md) for full Kafka documentation.

| Field | Type | Default |
|---|---|---|
| `output.kafka.brokers` | `[]string` | `[]` |
| `output.kafka.topic` | `string` | `""` |
| `output.kafka.max_events` | `int` | `1000` |
| `output.kafka.flush_interval` | `duration` | `5s` |
| `output.kafka.buffer_size` | `int` | `10000` |
| `output.kafka.client_id` | `string` | `snmp-collector` |
| `output.kafka.required_acks` | `string` | `local` |
| `output.kafka.max_retry` | `int` | `3` |
| `output.kafka.compression` | `string` | `none` |
| `output.kafka.version` | `string` | `""` |
| `output.kafka.tls.*` | — | all disabled |
| `output.kafka.sasl.*` | — | all disabled |

#### output.file

| Field | Type | Default | Description |
|---|---|---|---|
| `output.file.path` | `string` | `""` | Absolute or relative path to the output file. |
| `output.file.max_bytes` | `int64` | `52428800` (50 MB) | Rotate when the file exceeds this size. `0` disables rotation. |
| `output.file.max_backups` | `int` | `5` | Number of rotated files to keep. `0` keeps all. |

```yaml
output:
  file:
    path:        /var/log/snmpcollector/metrics.json
    max_bytes:   52428800
    max_backups: 5
```

---

## CLI Flags Reference

Every YAML field has a corresponding CLI flag. CLI flags use seconds (integer)
for duration fields; the YAML accepts Go duration strings (`30s`, `5m`, etc.).

| CLI flag | YAML key | Default |
|---|---|---|
| `-config` | — | `""` |
| `-log.level` | `log.level` | `info` |
| `-log.fmt` | `log.format` | `json` |
| `-collector.id` | `collector_id` | hostname |
| `-format.pretty` | `format.pretty` | `false` |
| `-format.otel` | `format.otel` | `false` |
| `-format.otel.scope-name` | `format.otel_scope_name` | `snmp-collector` |
| `-format.otel.scope-version` | `format.otel_scope_version` | `""` |
| `-pipeline.buffer.size` | `pipeline.buffer_size` | `10000` |
| `-pipeline.decode.workers` | `pipeline.decode_workers` | `1` |
| `-pipeline.produce.workers` | `pipeline.produce_workers` | `1` |
| `-poller.workers` | `poller.workers` | `500` |
| `-snmp.pool.max.idle` | `poller.pool.max_idle` | `2` |
| `-snmp.pool.idle.timeout` | `poller.pool.idle_timeout` | `30` |
| `-processor.enum.enable` | `processors.enum.enable` | `false` |
| `-processor.counter.delta` | `processors.counter.delta_enable` | `true` |
| `-processor.counter.purge.interval` | `processors.counter.purge_interval` | `300` |
| `-config.reload.interval` | `config_reload_interval` | `0` |
| `-config.devices` | `config_paths.devices` | env / default |
| `-config.device.groups` | `config_paths.device_groups` | env / default |
| `-config.object.groups` | `config_paths.object_groups` | env / default |
| `-config.objects` | `config_paths.objects` | env / default |
| `-config.enums` | `config_paths.enums` | env / default |
| `-health.addr` | `health.addr` | `""` |
| `-output.file` | `output.file.path` | `""` |
| `-output.file.max-bytes` | `output.file.max_bytes` | `52428800` |
| `-output.file.max-backups` | `output.file.max_backups` | `5` |
| `-output.kafka.brokers` | `output.kafka.brokers` | `""` |
| `-output.kafka.topic` | `output.kafka.topic` | `""` |
| `-output.kafka.max-events` | `output.kafka.max_events` | `1000` |
| `-output.kafka.flush-interval` | `output.kafka.flush_interval` | `5` |
| `-output.kafka.buffer-size` | `output.kafka.buffer_size` | `10000` |
| `-output.kafka.client-id` | `output.kafka.client_id` | `snmp-collector` |
| `-output.kafka.required-acks` | `output.kafka.required_acks` | `local` |
| `-output.kafka.max-retry` | `output.kafka.max_retry` | `3` |
| `-output.kafka.compression` | `output.kafka.compression` | `none` |
| `-output.kafka.version` | `output.kafka.version` | `""` |
| `-output.kafka.tls.enable` | `output.kafka.tls.enable` | `false` |
| `-output.kafka.tls.insecure-skip-verify` | `output.kafka.tls.insecure_skip_verify` | `false` |
| `-output.kafka.tls.ca-cert` | `output.kafka.tls.ca_cert` | `""` |
| `-output.kafka.tls.client-cert` | `output.kafka.tls.client_cert` | `""` |
| `-output.kafka.tls.client-key` | `output.kafka.tls.client_key` | `""` |
| `-output.kafka.sasl.enable` | `output.kafka.sasl.enable` | `false` |
| `-output.kafka.sasl.user` | `output.kafka.sasl.user` | `""` |
| `-output.kafka.sasl.password` | `output.kafka.sasl.password` | `""` |
| `-output.kafka.sasl.mechanism` | `output.kafka.sasl.mechanism` | `PLAIN` |

---

## Output Transport Selection

```
output.kafka.brokers non-empty?
    YES → Kafka transport (broker list from YAML or CLI)
    NO  →
        output.file.path non-empty?
            YES → File transport (rotating JSON file)
            NO  → Stdout (default — writes JSON to os.Stdout)
```

Only one transport is active per process. To fan out to multiple destinations,
run multiple collector instances.

---

## Environment Variables

These variables set the fallback paths for SNMP data directories when neither
the YAML file nor a CLI flag provides a value.

| Variable | Used by |
|---|---|
| `INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH` | `config_paths.devices` |
| `INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH` | `config_paths.device_groups` |
| `INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH` | `config_paths.object_groups` |
| `INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH` | `config_paths.objects` |
| `PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH` | `config_paths.enums` |

---

## Annotated Example File

A copy of this file is available at `config.example.yaml` in the repository
root. Copy it and remove the fields you do not need.

```yaml
# ── Logging ───────────────────────────────────────────────────────────────────
log:
  level: info     # debug | info | warn | error
  format: json    # json | text

# ── Identity ──────────────────────────────────────────────────────────────────
collector_id: ""  # defaults to system hostname

# ── Pipeline ──────────────────────────────────────────────────────────────────
pipeline:
  buffer_size: 10000
  decode_workers: 1
  produce_workers: 1

# ── Poller ────────────────────────────────────────────────────────────────────
poller:
  workers: 500
  pool:
    max_idle: 2
    idle_timeout: 30s

# ── Processors ────────────────────────────────────────────────────────────────
processors:
  enum:
    enable: false
  counter:
    delta_enable: true
    purge_interval: 5m

# ── Output format ─────────────────────────────────────────────────────────────
# Custom JSON (default):
format:
  pretty: false
# OpenTelemetry OTLP JSON (mutually exclusive with pretty):
# format:
#   otel: true
#   otel_scope_name: "snmp-collector"
#   otel_scope_version: ""

# ── Config management ─────────────────────────────────────────────────────────
config_reload_interval: 0s

config_paths:
  devices: ""        # falls back to INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH
  device_groups: ""
  object_groups: ""
  objects: ""
  enums: ""

# ── Health check ──────────────────────────────────────────────────────────────
health:
  addr: ""           # e.g. ":8080"

# ── Output ────────────────────────────────────────────────────────────────────
output:
  kafka:
    brokers: []      # non-empty activates Kafka output
    topic: ""
    max_events: 1000
    flush_interval: 5s
    buffer_size: 10000
    client_id: snmp-collector
    required_acks: local
    max_retry: 3
    compression: none
    version: ""
    tls:
      enable: false
      insecure_skip_verify: false
      ca_cert: ""
      client_cert: ""
      client_key: ""
    sasl:
      enable: false
      user: ""
      password: ""
      mechanism: PLAIN

  file:
    path: ""         # non-empty activates file output (if kafka.brokers is empty)
    max_bytes: 52428800
    max_backups: 5
```
