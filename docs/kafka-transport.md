# Kafka Transport

The Kafka transport (`plugin/transport/kafka`) delivers formatted SNMP metrics
to an Apache Kafka topic using a `sarama.SyncProducer` and an internal
buffered-channel batching worker.

---

## Table of Contents

- [Architecture](#architecture)
- [Flush Conditions](#flush-conditions)
- [Shutdown Behaviour](#shutdown-behaviour)
- [Configuration Reference](#configuration-reference)
  - [Core Settings](#core-settings)
  - [TLS Settings](#tls-settings)
  - [SASL Settings](#sasl-settings)
- [YAML Configuration](#yaml-configuration)
- [CLI Flags](#cli-flags)
- [Concurrency Contract](#concurrency-contract)
- [Error Handling](#error-handling)
- [Dependencies](#dependencies)

---

## Architecture

```
pipeline workers           kafka transport
─────────────────          ────────────────────────────────────────────
Send([]byte)   ──────────► buffered channel (BufferSize)
Send([]byte)   ──────────►        │
Send([]byte)   ──────────►        ▼
                           worker goroutine (single owner of batch)
                              │
                              ├─ data arrives → append to batch
                              │   └─ batch full? → SendMessages → Kafka
                              │
                              ├─ ticker fires → SendMessages → Kafka
                              │
                              └─ quit signal → drain channel → SendMessages → Kafka
                                                                      │
                                                               sarama.SyncProducer
                                                                (IBM/sarama)
```

**Key design decisions:**

| Decision | Reason |
|---|---|
| Single worker goroutine owns the batch slice | No mutex needed on the batch; race-free by construction |
| Buffered channel between Send and worker | Decouples fast Send callers from slower Kafka I/O; natural back-pressure |
| `sarama.SendMessages` for batch delivery | One network round-trip per batch instead of one per message |
| `quit` channel (not channel close) for shutdown | Avoids send-on-closed-channel panics; worker can drain after signal |

---

## Flush Conditions

A batch is sent to Kafka when **either** condition is met — whichever comes
first:

| Condition | Parameter | Default |
|---|---|---|
| **Size** — batch accumulates this many messages | `max_events` | `1000` |
| **Age** — this much time has elapsed since the last flush | `flush_interval` | `5s` |

The age-based flush guarantees that low-volume periods still push data through
quickly and do not leave stale metrics sitting in memory.

---

## Shutdown Behaviour

Triggered by `Transport.Close()` (called by the app after all pipeline workers
have stopped):

1. The `quit` channel is closed — any `Send` callers blocked waiting for buffer
   space return immediately with an error.
2. The worker detects `<-quit`, then **non-blocking-drains** whatever messages
   are already buffered in the channel (messages submitted before the quit
   signal).
3. The remaining partial batch is flushed to Kafka via `SendMessages`.
4. The `done` channel is closed to signal completion.
5. `Close` unblocks from `<-done` and tears down the `sarama.SyncProducer`.

This ensures **no messages are dropped** on graceful shutdown, even mid-batch.

---

## Configuration Reference

### Core Settings

| Field | Type | Default | Description |
|---|---|---|---|
| `brokers` | `[]string` | — | **Required.** Bootstrap broker addresses (`host:port`). |
| `topic` | `string` | — | **Required.** Kafka topic to publish to. |
| `max_events` | `int` | `1000` | Flush batch when it reaches this many messages. |
| `flush_interval` | `duration` | `5s` | Flush stale batches after this duration. |
| `buffer_size` | `int` | `10000` | Capacity of the internal channel. When full, `Send` blocks (back-pressure). |
| `client_id` | `string` | `snmp-collector` | Kafka client identifier sent to brokers (visible in broker logs). |
| `required_acks` | `string` | `local` | Producer acknowledgement level. See table below. |
| `max_retry` | `int` | `3` | Number of times sarama retries a failed send before surfacing an error. |
| `compression` | `string` | `none` | Message compression algorithm. See table below. |
| `version` | `string` | `""` | Kafka protocol version to negotiate (e.g. `3.6.0`). Empty = sarama default. |

**`required_acks` values:**

| Value | sarama constant | Behaviour |
|---|---|---|
| `none` | `NoResponse` | Fire and forget — highest throughput, no durability guarantee |
| `local` | `WaitForLocal` | Wait for the partition leader to acknowledge (default) |
| `all` | `WaitForAll` | Wait for all in-sync replicas — lowest throughput, strongest guarantee |

**`compression` values:** `none`, `gzip`, `snappy`, `lz4`, `zstd`

---

### TLS Settings

Configure TLS when brokers require encrypted connections.

| Field | Type | Default | Description |
|---|---|---|---|
| `tls.enable` | `bool` | `false` | Activate TLS for broker connections. |
| `tls.insecure_skip_verify` | `bool` | `false` | Skip broker certificate verification. **Never use in production.** |
| `tls.ca_cert` | `string` | `""` | Path to a PEM-encoded CA certificate used to verify the broker's certificate. |
| `tls.client_cert` | `string` | `""` | Path to a PEM-encoded client certificate (mTLS). Must be paired with `client_key`. |
| `tls.client_key` | `string` | `""` | Path to a PEM-encoded client private key (mTLS). Must be paired with `client_cert`. |

---

### SASL Settings

Configure SASL when brokers require authentication.

| Field | Type | Default | Description |
|---|---|---|---|
| `sasl.enable` | `bool` | `false` | Activate SASL authentication. |
| `sasl.user` | `string` | `""` | SASL username. |
| `sasl.password` | `string` | `""` | SASL password. |
| `sasl.mechanism` | `string` | `PLAIN` | SASL exchange mechanism. See table below. |

**`sasl.mechanism` values:**

| Value | Description |
|---|---|
| `PLAIN` | Cleartext credentials. Use only over TLS. |
| `SCRAM-SHA-256` | SCRAM challenge-response using SHA-256. Recommended. |
| `SCRAM-SHA-512` | SCRAM challenge-response using SHA-512. |

SCRAM mechanisms are implemented via `github.com/xdg-go/scram`. No additional
configuration is needed beyond setting the mechanism string.

---

## YAML Configuration

Minimal (plaintext brokers, no auth):

```yaml
output:
  kafka:
    brokers:
      - broker1:9092
      - broker2:9092
    topic: snmp-metrics
```

Production (mTLS + SCRAM, tuned batching):

```yaml
output:
  kafka:
    brokers:
      - kafka-a.internal:9093
      - kafka-b.internal:9093
      - kafka-c.internal:9093
    topic: snmp-metrics
    max_events: 500
    flush_interval: 2s
    buffer_size: 20000
    client_id: dc1-snmp-collector
    required_acks: all
    compression: zstd
    version: "3.6.0"

    tls:
      enable: true
      ca_cert:     /etc/ssl/kafka/ca.pem
      client_cert: /etc/ssl/kafka/collector.crt
      client_key:  /etc/ssl/kafka/collector.key

    sasl:
      enable: true
      user:      collector-svc
      password:  s3cr3t
      mechanism: SCRAM-SHA-256
```

---

## CLI Flags

All fields are also settable via CLI flags. Flags take precedence over the YAML
file. Duration fields use whole seconds for the CLI.

```
-output.kafka.brokers               "broker1:9092,broker2:9092"
-output.kafka.topic                 snmp-metrics
-output.kafka.max-events            1000
-output.kafka.flush-interval        5        (seconds)
-output.kafka.buffer-size           10000
-output.kafka.client-id             snmp-collector
-output.kafka.required-acks         local    (none|local|all)
-output.kafka.max-retry             3
-output.kafka.compression           none     (none|gzip|snappy|lz4|zstd)
-output.kafka.version               ""

-output.kafka.tls.enable                    false
-output.kafka.tls.insecure-skip-verify      false
-output.kafka.tls.ca-cert                   ""
-output.kafka.tls.client-cert               ""
-output.kafka.tls.client-key                ""

-output.kafka.sasl.enable                   false
-output.kafka.sasl.user                     ""
-output.kafka.sasl.password                 ""
-output.kafka.sasl.mechanism                PLAIN
```

Brokers on the CLI are comma-separated:

```bash
./snmpcollector \
  -output.kafka.brokers broker1:9092,broker2:9092 \
  -output.kafka.topic   snmp-metrics \
  -output.kafka.required-acks all \
  -output.kafka.compression zstd
```

---

## Concurrency Contract

| Method | Goroutine-safe? | Notes |
|---|---|---|
| `New` | — | Called once at startup. |
| `Send` | **Yes** | Called from multiple pipeline worker goroutines. Puts data into the buffered channel; blocks if full. |
| `Close` | **Yes** | Safe to call exactly once. Protected by `sync.Once`. |

The internal batch slice is **never shared** — only the worker goroutine reads
or writes it. `Send` only ever writes to the channel. This design eliminates
the need for a mutex on the hot path.

---

## Error Handling

| Scenario | Behaviour |
|---|---|
| Broker unreachable at startup | `New` returns an error; the process exits. |
| Transient send failure (network blip) | sarama retries up to `max_retry` times internally before surfacing an error. |
| Partial batch failure | `flush` unwraps the `sarama.ProducerErrors`, logs each failed message individually (topic, error), and logs a summary of how many messages in the batch failed. Delivery continues for subsequent batches. |
| Wholesale batch failure | `flush` logs the batch size and the raw error. |
| Buffer full at shutdown | Any `Send` call blocked waiting for space returns immediately with `"kafka transport: transport is closed"`. |
| Producer close error | Logged at ERROR level; returned from `Close`. |

---

## Dependencies

| Package | Version | Purpose |
|---|---|---|
| `github.com/IBM/sarama` | v1.47.0+ | Kafka client (SyncProducer) |
| `github.com/xdg-go/scram` | v1.2.0+ | SCRAM-SHA-256/512 SASL client |
| `github.com/xdg-go/pbkdf2` | v1.0.0+ | Transitive (SCRAM) |
| `github.com/xdg-go/stringprep` | v1.0.4+ | Transitive (SCRAM) |
