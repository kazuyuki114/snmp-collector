package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"snmp/snmp-collector/models"
	"snmp/snmp-collector/pkg/snmpcollector/config"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper: minimal YAML config tree that produces at least one PollJob
// ─────────────────────────────────────────────────────────────────────────────

func writeTestConfig(t *testing.T) config.Paths {
	t.Helper()
	base := t.TempDir()

	dirs := []string{"devices", "device_groups", "object_groups", "objects", "enums"}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// One device with all fields specified (no defaults file).
	writeYAML(t, filepath.Join(base, "devices", "dev1.yml"), `
testdevice:
  ip: 127.0.0.250
  port: 161
  poll_interval: 1
  timeout: 500
  retries: 0
  version: "2c"
  communities: ["public"]
  device_groups: ["testgroup"]
  max_concurrent_polls: 2
`)

	writeYAML(t, filepath.Join(base, "device_groups", "testgroup.yml"), `
testgroup:
  object_groups:
    - sysgroup
`)

	writeYAML(t, filepath.Join(base, "object_groups", "sysgroup.yml"), `
sysgroup:
  objects:
    - "SNMPv2-MIB::system"
`)

	writeYAML(t, filepath.Join(base, "objects", "system.yml"), `
SNMPv2-MIB::system:
  mib: SNMPv2-MIB
  object: system
  attributes:
    sysDescr:
      oid: ".1.3.6.1.2.1.1.1"
      name: "sys.descr"
      syntax: "DisplayString"
      tag: true
    sysUpTime:
      oid: ".1.3.6.1.2.1.1.3"
      name: "sys.uptime"
      syntax: "TimeTicks"
`)

	return config.Paths{
		Devices:      filepath.Join(base, "devices"),
		DeviceGroups: filepath.Join(base, "device_groups"),
		ObjectGroups: filepath.Join(base, "object_groups"),
		Objects:      filepath.Join(base, "objects"),
		Enums:        filepath.Join(base, "enums"),
	}
}

func writeYAML(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestNew_defaults(t *testing.T) {
	a := New(Config{}, nil)

	if a.cfg.PollerWorkers != 500 {
		t.Errorf("PollerWorkers = %d, want 500", a.cfg.PollerWorkers)
	}
	if a.cfg.BufferSize != 10_000 {
		t.Errorf("BufferSize = %d, want 10000", a.cfg.BufferSize)
	}
	if a.cfg.CollectorID == "" {
		t.Error("CollectorID should default to hostname, got empty")
	}
	if a.logger == nil {
		t.Error("logger should never be nil")
	}
}

func TestStartStop_emptyConfig(t *testing.T) {
	// The config loader silently skips nonexistent directories. An empty
	// config is valid — the scheduler simply has zero entries.
	a := New(Config{
		ConfigPaths:   config.Paths{}, // all defaults → nonexistent dirs → empty
		PollerWorkers: 1,
		BufferSize:    10,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	err := a.Start(ctx)
	if err != nil {
		cancel()
		t.Fatalf("Start with empty config: %v", err)
	}

	cancel()
	a.Stop()
}

func TestStartStop_lifecycle(t *testing.T) {
	paths := writeTestConfig(t)

	// Capture output via a buffer.
	var buf safeBuffer
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	a := New(Config{
		ConfigPaths:   paths,
		PollerWorkers: 2,
		BufferSize:    100,
		PrettyPrint:   false,
		Transport:     newWriterTransport(&buf, logger),
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())

	err := a.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The device (127.0.0.250) will fail to connect — that's fine.
	// We're testing that the pipeline starts and stops cleanly.
	// Give it a moment to attempt at least one poll cycle.
	time.Sleep(500 * time.Millisecond)

	cancel()
	a.Stop()

	// If we get here without hanging, the lifecycle is correct.
}

func TestReload(t *testing.T) {
	paths := writeTestConfig(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	a := New(Config{
		ConfigPaths:   paths,
		PollerWorkers: 2,
		BufferSize:    10,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	// Reload with same config — should succeed.
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

func TestPipelineIntegration_metricsFlowToTransport(t *testing.T) {
	// This test bypasses the poller entirely and injects raw data directly
	// into the pipeline channels to verify decode → produce → format → transport.

	paths := writeTestConfig(t)
	var buf safeBuffer
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	a := New(Config{
		ConfigPaths:   paths,
		PollerWorkers: 1,
		BufferSize:    100,
		Transport:     newWriterTransport(&buf, logger),
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	err := a.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Inject a synthetic metric directly into metricCh.
	// This tests formatter → transport.
	a.metricCh <- models.SNMPMetric{
		Timestamp: time.Date(2026, 2, 26, 10, 30, 0, 0, time.UTC),
		Device: models.Device{
			Hostname:    "testdevice",
			IPAddress:   "127.0.0.250",
			SNMPVersion: "2c",
		},
		Metrics: []models.Metric{
			{
				OID:    ".1.3.6.1.2.1.1.3.0",
				Name:   "sys.uptime",
				Value:  int64(123456),
				Type:   "TimeTicks",
				Syntax: "TimeTicks",
			},
		},
		Metadata: models.MetricMetadata{
			CollectorID:    "test",
			PollDurationMs: 5,
			PollStatus:     "success",
		},
	}

	// Give pipeline time to process.
	time.Sleep(200 * time.Millisecond)

	cancel()
	a.Stop()

	// Verify output.
	output := buf.String()
	if output == "" {
		t.Fatal("expected transport output, got empty")
	}

	// Parse the JSON to ensure it's valid.
	var result models.SNMPMetric
	if err := json.Unmarshal([]byte(firstLine(output)), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, output)
	}
	if result.Device.Hostname != "testdevice" {
		t.Errorf("hostname = %q, want testdevice", result.Device.Hostname)
	}
	if len(result.Metrics) != 1 {
		t.Errorf("metrics count = %d, want 1", len(result.Metrics))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Utilities
// ─────────────────────────────────────────────────────────────────────────────

// safeBuffer is a concurrency-safe bytes.Buffer for use as a transport writer.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// firstLine returns the first line from s.
func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
