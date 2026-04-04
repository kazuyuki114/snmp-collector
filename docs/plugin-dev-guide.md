# Writing New Plugins — Developer Guide

This guide explains how to add new **Input** and **Transport** plugins to the
SNMP Collector. The architecture follows a Telegraf-style model where every
plugin is a self-contained Go package that satisfies a small interface.

---

## Table of Contents

- [Writing New Plugins — Developer Guide](#writing-new-plugins--developer-guide)
  - [Table of Contents](#table-of-contents)
  - [Architecture Overview](#architecture-overview)
  - [Writing a New Input Plugin](#writing-a-new-input-plugin)
    - [Input Interface](#input-interface)
    - [Input Package Layout](#input-package-layout)
    - [Input Step-by-Step](#input-step-by-step)
      - [1. Create the package](#1-create-the-package)
      - [2. Define Config and the Input struct](#2-define-config-and-the-input-struct)
      - [3. Implement Name()](#3-implement-name)
      - [4. Implement Start()](#4-implement-start)
      - [5. Emit Envelopes](#5-emit-envelopes)
      - [6. Implement Stop()](#6-implement-stop)
    - [Example: SNMP Trap Input](#example-snmp-trap-input)
    - [Input Testing](#input-testing)
  - [Writing a New Transport Plugin](#writing-a-new-transport-plugin)
    - [Transport Interface](#transport-interface)
    - [Transport Package Layout](#transport-package-layout)
    - [Transport Step-by-Step](#transport-step-by-step)
      - [1. Create the package](#1-create-the-package-1)
      - [2. Define Config and the Transport struct](#2-define-config-and-the-transport-struct)
      - [3. Implement Name()](#3-implement-name-1)
      - [4. Implement Send()](#4-implement-send)
      - [5. Implement Close()](#5-implement-close)
    - [Example: Kafka Transport](#example-kafka-transport)
    - [Transport Testing](#transport-testing)
  - [Registering Plugins with the Engine](#registering-plugins-with-the-engine)
  - [Design Rules](#design-rules)
    - [Dependency graph](#dependency-graph)
  - [Existing Plugins Reference](#existing-plugins-reference)
    - [Input: SNMP Poller (`plugin/input/snmppoller/`)](#input-snmp-poller-plugininputsnmppoller)
    - [Transport: Stdout (`plugin/transport/stdout/`)](#transport-stdout-plugintransportstdout)
    - [Transport: File (`plugin/transport/file/`)](#transport-file-plugintransportfile)

---

## Architecture Overview

```
Input₁ ─┐                            ┌─▶ Transport₁
Input₂ ─┤──▶ [envelopeCh] ──▶ Fmt ──┤──▶ Transport₂
InputN ─┘                            └─▶ TransportN
```

- **Inputs** collect data and emit `plugin.Envelope` values on a shared channel.
- The **Engine** reads envelopes, formats them to JSON, and fans out to all **Transports**.
- Each plugin is an independent Go package under `plugin/input/` or `plugin/transport/`.
- Plugins depend only on the `plugin` package (for interfaces) and optionally on
  shared libraries (`snmp/decoder`, `producer/metrics`, etc.). They never import
  other plugins.

---

## Writing a New Input Plugin

### Input Interface

Every Input must implement `plugin.Input` (defined in `plugin/input.go`):

```go
type Input interface {
    // Name returns a unique identifier (e.g. "snmp_trap", "gnmi").
    Name() string

    // Start begins data collection. Send Envelopes on `out`.
    // Must return quickly — launch goroutines for long-running work.
    // Respect ctx cancellation.
    Start(ctx context.Context, out chan<- Envelope) error

    // Stop performs graceful shutdown. Blocks until all goroutines exit.
    // After Stop returns, no more Envelopes may be sent.
    Stop()
}
```

### Input Package Layout

```
plugin/input/<name>/
├── <name>.go       # Config, Input struct, New(), Name(), Start(), Stop()
├── <name>_test.go  # Unit tests
└── (optional)      # Helper files (handler.go, decode.go, etc.)
```

### Input Step-by-Step

#### 1. Create the package

```bash
mkdir -p plugin/input/snmptrap
```

#### 2. Define Config and the Input struct

```go
package snmptrap

import (
    "context"
    "log/slog"
    "sync"

    "github.com/snmp/snmp_collector/plugin"
)

// compile-time interface check.
var _ plugin.Input = (*Input)(nil)

type Config struct {
    ListenAddr string // e.g. ":162"
    Community  string
    // ... plugin-specific settings
}

type Input struct {
    cfg    Config
    logger *slog.Logger
    cancel context.CancelFunc
    wg     sync.WaitGroup
}

func New(cfg Config, logger *slog.Logger) *Input {
    return &Input{cfg: cfg, logger: logger}
}
```

**Key points:**
- Compile-time check: `var _ plugin.Input = (*Input)(nil)`
- Accept `*slog.Logger` — fall back to a no-op logger if nil.
- Store a `context.CancelFunc` and `sync.WaitGroup` for lifecycle management.

#### 3. Implement Name()

```go
func (i *Input) Name() string { return "snmp_trap" }
```

The name must be unique across all registered inputs. It appears in logs and in
`Envelope.Source`.

#### 4. Implement Start()

```go
func (i *Input) Start(ctx context.Context, out chan<- plugin.Envelope) error {
    // Validate config
    if i.cfg.ListenAddr == "" {
        return fmt.Errorf("snmp_trap: ListenAddr is required")
    }

    // Create a child context so Stop() can cancel it
    childCtx, cancel := context.WithCancel(ctx)
    i.cancel = cancel

    // Launch background goroutine(s)
    i.wg.Add(1)
    go i.listen(childCtx, out)

    i.logger.Info("snmp_trap: started", "addr", i.cfg.ListenAddr)
    return nil
}
```

**Rules for Start():**
- **Return quickly.** All blocking work (listeners, poll loops) must run in
  background goroutines tracked by `i.wg`.
- **Wrap the context.** Create a child context so `Stop()` can cancel it
  independently of the parent.
- **Return an error** only if the plugin cannot initialise at all (bad config,
  port in use). The engine logs the error and moves on.

#### 5. Emit Envelopes

Inside your goroutine, produce `plugin.Envelope` values and send them on `out`:

```go
func (i *Input) listen(ctx context.Context, out chan<- plugin.Envelope) {
    defer i.wg.Done()

    for {
        // ... receive data from your source ...

        env := plugin.Envelope{
            Source:    i.Name(),
            Timestamp: time.Now(),
            Metric:    &metric, // *models.SNMPMetric
        }

        select {
        case out <- env:
        case <-ctx.Done():
            return
        }
    }
}
```

**Rules for sending:**
- Always use a `select` with `ctx.Done()` to avoid blocking forever.
- `Envelope.Source` must match `Name()`.
- `Envelope.Metric` must not be nil (the engine drops envelopes where
  `Valid()` returns false).

#### 6. Implement Stop()

```go
func (i *Input) Stop() {
    i.logger.Info("snmp_trap: shutting down")
    if i.cancel != nil {
        i.cancel()     // signal goroutines to exit
    }
    i.wg.Wait()        // wait for all goroutines to finish
    // Clean up resources (close listeners, connections, etc.)
    i.logger.Info("snmp_trap: shutdown complete")
}
```

**Rules for Stop():**
- Must block until all goroutines have exited.
- Must be idempotent (safe to call twice).
- After Stop returns, the plugin must not send anything on `out`.

### Example: SNMP Trap Input

A complete skeleton for a trap receiver input:

```go
package snmptrap

import (
    "context"
    "fmt"
    "log/slog"
    "net"
    "sync"
    "time"

    "github.com/gosnmp/gosnmp"
    "github.com/snmp/snmp_collector/models"
    "github.com/snmp/snmp_collector/plugin"
)

var _ plugin.Input = (*Input)(nil)

type Config struct {
    ListenAddr  string // ":162"
    Community   string // "public"
    CollectorID string
}

type Input struct {
    cfg      Config
    logger   *slog.Logger
    listener *gosnmp.TrapListener
    cancel   context.CancelFunc
    wg       sync.WaitGroup
}

func New(cfg Config, logger *slog.Logger) *Input {
    if logger == nil {
        logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
    }
    return &Input{cfg: cfg, logger: logger}
}

func (i *Input) Name() string { return "snmp_trap" }

func (i *Input) Start(ctx context.Context, out chan<- plugin.Envelope) error {
    if i.cfg.ListenAddr == "" {
        return fmt.Errorf("snmp_trap: ListenAddr is required")
    }

    childCtx, cancel := context.WithCancel(ctx)
    i.cancel = cancel

    i.listener = gosnmp.NewTrapListener()
    i.listener.Params = gosnmp.Default
    i.listener.Params.Community = i.cfg.Community

    i.listener.OnNewTrap = func(pkt *gosnmp.SnmpPacket, addr *net.UDPAddr) {
        metric := i.processTrap(pkt, addr)
        if metric == nil {
            return
        }
        env := plugin.Envelope{
            Source:    i.Name(),
            Timestamp: time.Now(),
            Metric:    metric,
        }
        select {
        case out <- env:
        case <-childCtx.Done():
        }
    }

    i.wg.Add(1)
    go func() {
        defer i.wg.Done()
        i.logger.Info("snmp_trap: listening", "addr", i.cfg.ListenAddr)
        if err := i.listener.Listen(i.cfg.ListenAddr); err != nil {
            i.logger.Error("snmp_trap: listener error", "error", err.Error())
        }
    }()

    // Background goroutine to stop listener on context cancellation.
    i.wg.Add(1)
    go func() {
        defer i.wg.Done()
        <-childCtx.Done()
        i.listener.Close()
    }()

    return nil
}

func (i *Input) Stop() {
    i.logger.Info("snmp_trap: shutting down")
    if i.cancel != nil {
        i.cancel()
    }
    i.wg.Wait()
    i.logger.Info("snmp_trap: shutdown complete")
}

func (i *Input) processTrap(pkt *gosnmp.SnmpPacket, addr *net.UDPAddr) *models.SNMPMetric {
    // Convert trap PDU variables into an SNMPMetric.
    // This is where you decode varbinds, resolve OIDs, etc.
    // Return nil to silently drop malformed traps.
    return nil // TODO: implement
}

type noopWriter struct{}
func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
```

### Input Testing

```go
package snmptrap

import (
    "context"
    "testing"
    "time"

    "github.com/snmp/snmp_collector/models"
    "github.com/snmp/snmp_collector/plugin"
)

func TestInput_ImplementsInterface(t *testing.T) {
    var _ plugin.Input = (*Input)(nil)
}

func TestInput_Name(t *testing.T) {
    in := New(Config{}, nil)
    if in.Name() != "snmp_trap" {
        t.Errorf("expected snmp_trap, got %s", in.Name())
    }
}

func TestInput_StartStop(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    out := make(chan plugin.Envelope, 100)
    in := New(Config{ListenAddr: "127.0.0.1:10162", Community: "test"}, nil)

    if err := in.Start(ctx, out); err != nil {
        t.Fatalf("Start: %v", err)
    }

    // Give it a moment to start listening, then shut down.
    time.Sleep(100 * time.Millisecond)
    in.Stop()
}
```

---

## Writing a New Transport Plugin

### Transport Interface

Every Transport must implement `plugin.Transport` (defined in `plugin/transport.go`):

```go
type Transport interface {
    // Name returns a unique identifier (e.g. "kafka", "influxdb").
    Name() string

    // Send delivers one pre-formatted message (JSON bytes).
    // MUST be safe for concurrent use.
    Send(data []byte) error

    // Close flushes and releases resources. Called once during shutdown.
    Close() error
}
```

### Transport Package Layout

```
plugin/transport/<name>/
├── <name>.go       # Config, Transport struct, New(), Name(), Send(), Close()
├── <name>_test.go  # Unit tests
└── (optional)      # Helper files (rotate.go, batch.go, etc.)
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

    "github.com/snmp/snmp_collector/plugin"
)

var _ plugin.Transport = (*Transport)(nil)

type Config struct {
    Brokers []string
    Topic   string
    // ... plugin-specific settings
}

type Transport struct {
    mu     sync.Mutex
    cfg    Config
    logger *slog.Logger
    // client *kafka.Producer  // your Kafka client
}

func New(cfg Config, logger *slog.Logger) (*Transport, error) {
    // Validate config, create client, etc.
    return &Transport{cfg: cfg, logger: logger}, nil
}
```

**Key points:**
- Compile-time check: `var _ plugin.Transport = (*Transport)(nil)`
- `New()` can return an error (unlike Input — transports often need to
  establish connections up front).
- Include a `sync.Mutex` — `Send()` is called concurrently.

#### 3. Implement Name()

```go
func (t *Transport) Name() string { return "kafka" }
```

#### 4. Implement Send()

```go
func (t *Transport) Send(data []byte) error {
    t.mu.Lock()
    defer t.mu.Unlock()

    // Write data to your destination.
    // Retry transient errors internally.
    // Return permanent errors immediately.
    return nil
}
```

**Rules for Send():**
- **Must be goroutine-safe.** The engine calls `Send` from multiple format
  workers concurrently.
- `data` is a complete JSON message (one envelope, already formatted).
- Transient errors (network blips) should be retried internally.
- Permanent errors should be returned so the engine can log them.

#### 5. Implement Close()

```go
func (t *Transport) Close() error {
    t.mu.Lock()
    defer t.mu.Unlock()

    // Flush buffers, close connections, release resources.
    t.logger.Info("kafka: closed")
    return nil
}
```

**Rules for Close():**
- Called exactly once, after all `Send` calls have returned.
- Flush any buffered data before returning.

### Example: Kafka Transport

The Kafka transport is fully implemented at `plugin/transport/kafka/`.
See [kafka-transport.md](kafka-transport.md) for its complete documentation.

Its design illustrates the recommended pattern for transports that need
**internal buffering and batching**:

```
Send([]byte)  ──►  buffered channel  ──►  single worker goroutine  ──►  sarama.SendMessages
```

Key points from the real implementation:

- **No mutex on the hot path.** The worker goroutine is the sole owner of the
  batch slice; `Send` only writes to the channel.
- **Two flush triggers** inside the worker's `for/select`: size limit
  (`MaxEvents`) and age limit (`FlushInterval` ticker).
- **Graceful drain on shutdown.** When `Close` closes the `quit` channel, the
  worker non-blocking-drains remaining channel items before flushing the last
  batch.
- **`sync.Once` in `Close`** prevents double-close panics even if the engine
  calls it more than once.

```go
// Simplified structure — see plugin/transport/kafka/kafka.go for full code.

type Transport struct {
    cfg      Config
    producer sarama.SyncProducer
    logger   *slog.Logger
    ch       chan []byte    // decouples Send from the worker
    quit     chan struct{}  // closed by Close to signal shutdown
    done     chan struct{}  // closed by worker when it exits
    closeOnce sync.Once
}

func (t *Transport) Send(data []byte) error {
    msg := make([]byte, len(data)) // copy — caller may reuse the slice
    copy(msg, data)
    select {
    case t.ch <- msg:
        return nil
    case <-t.quit:
        return errors.New("kafka transport: transport is closed")
    }
}

func (t *Transport) worker() {
    defer close(t.done)
    ticker := time.NewTicker(t.cfg.FlushInterval)
    defer ticker.Stop()
    var batch []*sarama.ProducerMessage
    for {
        select {
        case data := <-t.ch:
            batch = append(batch, t.makeMessage(data))
            if len(batch) >= t.cfg.MaxEvents {
                t.flush(batch); batch = batch[:0]
            }
        case <-ticker.C:
            if len(batch) > 0 {
                t.flush(batch); batch = batch[:0]
            }
        case <-t.quit:
            for { // drain remaining buffered items
                select {
                case data := <-t.ch:
                    batch = append(batch, t.makeMessage(data))
                default:
                    if len(batch) > 0 { t.flush(batch) }
                    return
                }
            }
        }
    }
}
```

### Transport Testing

```go
package kafka

import (
    "testing"

    "github.com/snmp/snmp_collector/plugin"
)

func TestTransport_ImplementsInterface(t *testing.T) {
    var _ plugin.Transport = (*Transport)(nil)
}

func TestTransport_Name(t *testing.T) {
    tr, _ := New(Config{Brokers: []string{"localhost:9092"}, Topic: "test"}, nil)
    if tr.Name() != "kafka" {
        t.Errorf("expected kafka, got %s", tr.Name())
    }
}

func TestNew_MissingBrokers(t *testing.T) {
    _, err := New(Config{Topic: "test"}, nil)
    if err == nil {
        t.Error("expected error for empty brokers")
    }
}

func TestSend_BuffersAndFlushes(t *testing.T) {
    tr, err := New(Config{
        Brokers:   []string{"localhost:9092"},
        Topic:     "test",
        BatchSize: 2,
    }, nil)
    if err != nil {
        t.Fatal(err)
    }

    _ = tr.Send([]byte(`{"a":1}`))
    if len(tr.buf) != 1 {
        t.Errorf("expected 1 buffered, got %d", len(tr.buf))
    }

    _ = tr.Send([]byte(`{"a":2}`))
    // After reaching BatchSize, buffer should be flushed
    if len(tr.buf) != 0 {
        t.Errorf("expected 0 after flush, got %d", len(tr.buf))
    }
}

func TestClose_FlushesRemaining(t *testing.T) {
    tr, _ := New(Config{
        Brokers:   []string{"localhost:9092"},
        Topic:     "test",
        BatchSize: 100,
    }, nil)
    _ = tr.Send([]byte(`{"a":1}`))

    if err := tr.Close(); err != nil {
        t.Fatalf("Close: %v", err)
    }
    if len(tr.buf) != 0 {
        t.Errorf("expected 0 after Close, got %d", len(tr.buf))
    }
}
```

---

## Registering Plugins with the Engine

After writing your plugin, wire it into the engine in `main.go` (or wherever
the engine is constructed):

```go
import (
    "github.com/snmp/snmp_collector/plugin/engine"
    "github.com/snmp/snmp_collector/plugin/input/snmppoller"
    "github.com/snmp/snmp_collector/plugin/input/snmptrap"
    "github.com/snmp/snmp_collector/plugin/transport/stdout"
    "github.com/snmp/snmp_collector/plugin/transport/file"
    "github.com/snmp/snmp_collector/plugin/transport/kafka"
)

func buildEngine(logger *slog.Logger) (*engine.Engine, error) {
    eng := engine.New(engine.Config{
        BufferSize:    10_000,
        FormatWorkers: 50,
    }, logger)

    // ── Inputs ──────────────────────────────────────────────────────
    eng.AddInput(snmppoller.New(snmppoller.Config{
        ConfigPaths:   paths,
        PollerWorkers: 500,
    }, logger))

    eng.AddInput(snmptrap.New(snmptrap.Config{
        ListenAddr: ":162",
        Community:  "public",
    }, logger))

    // ── Transports ──────────────────────────────────────────────────
    eng.AddTransport(stdout.New(stdout.Config{}, logger))

    fileTr, err := file.New(file.Config{
        FilePath:   "/var/log/snmp/metrics.json",
        MaxBytes:   100 * 1024 * 1024, // 100 MB
        MaxBackups: 5,
    }, logger)
    if err != nil {
        return nil, err
    }
    eng.AddTransport(fileTr)

    kafkaTr, err := kafka.New(kafka.Config{
        Brokers: []string{"kafka-1:9092"},
        Topic:   "snmp.metrics",
    }, logger)
    if err != nil {
        return nil, err
    }
    eng.AddTransport(kafkaTr)

    return eng, nil
}
```

The engine handles everything else: channel creation, formatting, fan-out, and
graceful shutdown.

---

## Design Rules

| Rule | Why |
|------|-----|
| **One package per plugin** | Keeps plugins isolated; each is independently testable and deployable |
| **Never import another plugin** | Prevents circular dependencies and coupling |
| **Only import `plugin` for interfaces** | The `plugin` package is the shared contract; everything else is private |
| **Shared libraries are OK** | Importing `snmp/decoder`, `producer/metrics`, or `models` is fine — they are domain libraries, not plugins |
| **`New()` accepts `Config` + `*slog.Logger`** | Consistent constructor pattern across all plugins |
| **Compile-time interface check** | `var _ plugin.Input = (*Input)(nil)` catches missing methods at build time |
| **`Send()` must be goroutine-safe** | The engine calls `Send` from multiple workers concurrently |
| **`Start()` must return quickly** | Launch goroutines for blocking work; return errors only for fatal init failures |
| **`Stop()` must block until clean** | The engine relies on `Stop()` completing before closing transports |
| **Log prefix matches `Name()`** | e.g. `snmp_trap: listening`, `kafka: flushed` — makes log grep easy |

### Dependency graph

```
plugin/                        ← interfaces only (Input, Transport, Envelope)
  ├── input/snmppoller/        ← depends on: plugin, snmp/decoder, producer/metrics, ...
  ├── input/snmptrap/          ← depends on: plugin, gosnmp, models
  ├── transport/stdout/        ← depends on: plugin (+ stdlib)
  ├── transport/file/          ← depends on: plugin (+ stdlib)
  └── transport/kafka/         ← depends on: plugin, sarama
```

No arrows between sibling packages. That's the goal.

---

## Existing Plugins Reference

### Input: SNMP Poller (`plugin/input/snmppoller/`)

| File | Purpose |
|------|---------|
| `snmppoller.go` | Full implementation: Config, Input struct, Start/Stop, internal pipeline (scheduler → workers → decoder → producer → Envelope) |

**Config fields:** `ConfigPaths`, `CollectorID`, `PollerWorkers`, `BufferSize`, `PoolOptions`, `EnumEnabled`, `CounterDeltaEnabled`

**Internal pipeline:**
```
Scheduler → WorkerPool → [rawCh] → Decoder → [decodedCh] → Producer → Envelope → out
```

### Transport: Stdout (`plugin/transport/stdout/`)

| File | Purpose |
|------|---------|
| `stdout.go` | Writes JSON + newline to `os.Stdout`. Zero config needed. |

**Config fields:** `Writer` (override for testing), `Newline` (default `"\n"`)

### Transport: File (`plugin/transport/file/`)

| File | Purpose |
|------|---------|
| `file.go` | Writes JSON + newline to a rotating file |
| `rotate.go` | Self-contained `RotatingFile` (size-based rotation, backup pruning) |

**Config fields:** `FilePath`, `MaxBytes`, `MaxBackups`, `Newline`

**Rotation scheme:** `metrics.json` → `metrics.json.1` → `metrics.json.2` → pruned

### Transport: Kafka (`plugin/transport/kafka/`)

| File | Purpose |
|------|---------|
| `kafka.go` | `Config`, `Transport`, `New`, `Send`, `Close`, batching worker, sarama config builder |
| `scram.go` | `scramClient` — implements `sarama.SCRAMClient` for SCRAM-SHA-256/512 SASL |

**Config fields:** `Brokers`, `Topic`, `MaxEvents`, `FlushInterval`, `BufferSize`,
`ClientID`, `RequiredAcks`, `MaxRetry`, `Compression`, `Version`, `TLS`, `SASL`

**Batching model:** worker goroutine collects into a `[]*sarama.ProducerMessage`
slice and calls `producer.SendMessages` when `MaxEvents` is reached or
`FlushInterval` elapses — whichever comes first.

**Security:** TLS (one-way and mTLS) + SASL (PLAIN, SCRAM-SHA-256, SCRAM-SHA-512).

See [kafka-transport.md](kafka-transport.md) for full documentation.
