# Writing New Plugins — Developer Guide

This guide explains how to add new **Transport** plugins to the SNMP Collector.
The collector currently exposes transport as the plugin extension point, while
collection/decoding/production remain internal pipeline stages.

---

## Table of Contents

- [Writing New Plugins — Developer Guide](#writing-new-plugins--developer-guide)
  - [Table of Contents](#table-of-contents)
  - [Architecture Overview](#architecture-overview)
  - [Writing a New Transport Plugin](#writing-a-new-transport-plugin)
    - [Transport Interface](#transport-interface)
    - [Transport Package Layout](#transport-package-layout)
    - [Transport Step-by-Step](#transport-step-by-step)
      - [1. Create the package](#1-create-the-package)
      - [2. Define Config and the Transport struct](#2-define-config-and-the-transport-struct)
      - [3. Implement Name()](#3-implement-name)
      - [4. Implement Send()](#4-implement-send)
      - [5. Implement Close()](#5-implement-close)
    - [Example: Kafka Transport](#example-kafka-transport)
    - [Transport Testing](#transport-testing)
  - [Wiring Transport in Main](#wiring-transport-in-main)
  - [Design Rules](#design-rules)
  - [Existing Plugins Reference](#existing-plugins-reference)
    - [Transport: Stdout (default)](#transport-stdout-default)
    - [Transport: File (`plugin/transport/file/`)](#transport-file-plugintransportfile)
    - [Transport: Kafka (`plugin/transport/kafka/`)](#transport-kafka-plugintransportkafka)

---

## Architecture Overview

```
Scheduler -> WorkerPool -> Decoder -> Producer -> Formatter -> Transport
```

- Polling and metric production are internal components in `pkg/snmpcollector/`.
- `plugin.Transport` is the stable extension interface for output delivery.
- `plugin.Envelope` remains a shared message type for pipeline internals.

---

## Writing a New Transport Plugin

### Transport Interface

Every transport must implement `plugin.Transport` (defined in `plugin/transport.go`):

```go
type Transport interface {
    // Name returns a unique identifier (for example "kafka", "file").
    Name() string

    // Send delivers one pre-formatted message (JSON bytes).
    // Must be safe for concurrent use.
    Send(data []byte) error

    // Close flushes and releases resources. Called once during shutdown.
    Close() error
}
```

### Transport Package Layout

```
plugin/transport/<name>/
|- <name>.go       # Config, Transport struct, New(), Name(), Send(), Close()
|- <name>_test.go  # Unit tests
`- (optional)      # Helper files (rotate.go, batch.go, tls.go, etc.)
```

### Transport Step-by-Step

#### 1. Create the package

```bash
mkdir -p plugin/transport/kafka
```

#### 2. Define Config and the Transport struct

```go
package kafka

import (
    "log/slog"
    "sync"

    "snmp/snmp-collector/plugin"
)

var _ plugin.Transport = (*Transport)(nil)

type Config struct {
    Brokers []string
    Topic   string
}

type Transport struct {
    mu     sync.Mutex
    cfg    Config
    logger *slog.Logger
}

func New(cfg Config, logger *slog.Logger) (*Transport, error) {
    if logger == nil {
        logger = slog.Default()
    }
    return &Transport{cfg: cfg, logger: logger}, nil
}
```

Key points:
- Keep the compile-time check: `var _ plugin.Transport = (*Transport)(nil)`
- `New()` may return an error for invalid config or failed client setup.
- Make `Send()` goroutine-safe.

#### 3. Implement Name()

```go
func (t *Transport) Name() string { return "kafka" }
```

#### 4. Implement Send()

```go
func (t *Transport) Send(data []byte) error {
    t.mu.Lock()
    defer t.mu.Unlock()

    // Deliver data to your destination.
    // Retry transient failures internally when appropriate.
    return nil
}
```

Rules for `Send()`:
- Must be goroutine-safe.
- Input `data` is already formatted by the formatter stage.
- Return permanent errors so the app can log/report them.

#### 5. Implement Close()

```go
func (t *Transport) Close() error {
    t.mu.Lock()
    defer t.mu.Unlock()

    // Flush and close underlying resources.
    return nil
}
```

Rules for `Close()`:
- Called after pipeline drain.
- Should flush buffered data before returning.
- Should be idempotent when possible.

### Example: Kafka Transport

The Kafka transport in `plugin/transport/kafka/` demonstrates a high-throughput
pattern:

```
Send([]byte) -> buffered channel -> worker goroutine -> batched SendMessages
```

Notable behavior:
- `Send()` is non-blocking relative to broker round-trips.
- Worker flushes on either max batch size or max age.
- `Close()` signals worker shutdown and drains any buffered payloads.

See `docs/kafka-transport.md` for complete details.

### Transport Testing

Recommended test coverage:
- Interface compliance (`var _ plugin.Transport = (*Transport)(nil)`)
- Config validation failures in `New()`
- Concurrent `Send()` safety
- Flush-on-close behavior
- Error propagation for permanent send failures

---

## Wiring Transport in Main

Transports are configured in the collector startup path (`cmd/snmpcollector/main.go`):

- YAML mode: exactly one output block should have `enabled: true`
- CLI mode: priority is Kafka brokers, then file path, otherwise stdout

To add a new transport:
1. Create `plugin/transport/<name>/` with `New(Config, *slog.Logger)`.
2. Construct it in startup based on config flags/YAML.
3. Assign it to app config as `cfg.Transport`.

---

## Design Rules

| Rule | Why |
|------|-----|
| One package per transport | Keeps each implementation isolated and testable |
| Never import another transport package | Avoids coupling and circular dependencies |
| Shared contract through `plugin.Transport` | Stable integration point for app startup |
| `New()` accepts `Config` + `*slog.Logger` | Consistent constructor style |
| `Send()` must be goroutine-safe | Multiple pipeline workers may call concurrently |
| `Close()` should drain cleanly | Prevents data loss on shutdown |

---

## Existing Plugins Reference

### Transport: Stdout (default)

Implemented as the in-app default writer transport in `pkg/snmpcollector/app/app.go`
when no custom transport is configured.

### Transport: File (`plugin/transport/file/`)

- `file.go`: write JSON lines to a file
- `rotate.go`: size-based rotation and backup pruning

### Transport: Kafka (`plugin/transport/kafka/`)

- `kafka.go`: producer setup, buffering, batching, send/close lifecycle
- `scram.go`: SCRAM auth helper

Supports TLS and SASL, plus configurable batching and compression.
