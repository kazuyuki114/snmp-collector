package scheduler_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"snmp/snmp-collector/models"
	"snmp/snmp-collector/pkg/snmpcollector/config"
	"snmp/snmp-collector/pkg/snmpcollector/poller"
	"snmp/snmp-collector/pkg/snmpcollector/scheduler"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock JobSubmitter
// ─────────────────────────────────────────────────────────────────────────────

type mockSubmitter struct {
	mu       sync.Mutex
	jobs     []poller.PollJob
	capacity int // 0 = unlimited
}

func newMockSubmitter(capacity int) *mockSubmitter {
	return &mockSubmitter{capacity: capacity}
}

func (m *mockSubmitter) Submit(job poller.PollJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs = append(m.jobs, job)
}

func (m *mockSubmitter) TrySubmit(job poller.PollJob) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.capacity > 0 && len(m.jobs) >= m.capacity {
		return false
	}
	m.jobs = append(m.jobs, job)
	return true
}

func (m *mockSubmitter) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jobs)
}

func (m *mockSubmitter) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs = nil
}

func (m *mockSubmitter) getJobs() []poller.PollJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]poller.PollJob, len(m.jobs))
	copy(cp, m.jobs)
	return cp
}

// ─────────────────────────────────────────────────────────────────────────────
// Test config builders
// ─────────────────────────────────────────────────────────────────────────────

func basicConfig() *config.LoadedConfig {
	return &config.LoadedConfig{
		Devices: map[string]config.DeviceConfig{
			"switch1": {
				IP:           "10.0.0.1",
				Port:         161,
				PollInterval: 1, // 1 second for fast tests
				Timeout:      500,
				Retries:      0,
				Version:      "2c",
				Communities:  []string{"public"},
				DeviceGroups: []string{"group_a"},
			},
		},
		DeviceGroups: map[string]config.DeviceGroup{
			"group_a": {ObjectGroups: []string{"og_netif"}},
		},
		ObjectGroups: map[string]config.ObjectGroup{
			"og_netif": {Objects: []string{"IF-MIB::ifEntry"}},
		},
		ObjectDefs: map[string]models.ObjectDefinition{
			"IF-MIB::ifEntry": {
				Key:    "IF-MIB::ifEntry",
				MIB:    "IF-MIB",
				Object: "ifEntry",
				Index: []models.IndexDefinition{
					{Type: "Integer", OID: ".1.3.6.1.2.1.2.2.1.1", Name: "netif"},
				},
				Attributes: map[string]models.AttributeDefinition{
					"ifInOctets": {
						OID:    ".1.3.6.1.2.1.2.2.1.10",
						Name:   "netif.bytes.in",
						Syntax: "Counter32",
					},
				},
			},
		},
	}
}

func multiDeviceConfig() *config.LoadedConfig {
	cfg := basicConfig()
	cfg.Devices["router1"] = config.DeviceConfig{
		IP:           "10.0.0.2",
		Port:         161,
		PollInterval: 2, // different interval
		Timeout:      500,
		Retries:      0,
		Version:      "2c",
		Communities:  []string{"public"},
		DeviceGroups: []string{"group_a"},
	}
	return cfg
}

// ─────────────────────────────────────────────────────────────────────────────
// ResolveJobs tests
// ─────────────────────────────────────────────────────────────────────────────

func TestResolveJobs(t *testing.T) {
	cfg := basicConfig()
	jobs := scheduler.ResolveJobs(cfg, nil)

	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.Hostname != "switch1" {
		t.Errorf("Hostname = %q, want %q", j.Hostname, "switch1")
	}
	if j.Device.IPAddress != "10.0.0.1" {
		t.Errorf("IPAddress = %q, want %q", j.Device.IPAddress, "10.0.0.1")
	}
	if j.ObjectDef.Key != "IF-MIB::ifEntry" {
		t.Errorf("ObjectDef.Key = %q, want %q", j.ObjectDef.Key, "IF-MIB::ifEntry")
	}
}

func TestResolveJobs_MultipleDevices(t *testing.T) {
	cfg := multiDeviceConfig()
	jobs := scheduler.ResolveJobs(cfg, nil)

	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	// Output is sorted by hostname.
	if jobs[0].Hostname != "router1" {
		t.Errorf("jobs[0].Hostname = %q, want %q", jobs[0].Hostname, "router1")
	}
	if jobs[1].Hostname != "switch1" {
		t.Errorf("jobs[1].Hostname = %q, want %q", jobs[1].Hostname, "switch1")
	}
}

func TestResolveJobs_Dedup(t *testing.T) {
	cfg := basicConfig()
	// Add a second device group that references the same object group.
	cfg.DeviceGroups["group_b"] = config.DeviceGroup{ObjectGroups: []string{"og_netif"}}
	cfg.Devices["switch1"] = config.DeviceConfig{
		IP:           "10.0.0.1",
		Port:         161,
		PollInterval: 60,
		Version:      "2c",
		Communities:  []string{"public"},
		DeviceGroups: []string{"group_a", "group_b"},
	}

	jobs := scheduler.ResolveJobs(cfg, nil)
	if len(jobs) != 1 {
		t.Errorf("got %d jobs, want 1 (duplicate object should be deduped)", len(jobs))
	}
}

func TestResolveJobs_MissingGroup(t *testing.T) {
	cfg := basicConfig()
	cfg.Devices["switch1"] = config.DeviceConfig{
		IP:           "10.0.0.1",
		Port:         161,
		PollInterval: 60,
		Version:      "2c",
		DeviceGroups: []string{"nonexistent_group"},
	}

	// Should not panic, just return 0 jobs.
	jobs := scheduler.ResolveJobs(cfg, nil)
	if len(jobs) != 0 {
		t.Errorf("got %d jobs, want 0 for missing group", len(jobs))
	}
}

func TestResolveJobs_MissingObjectDef(t *testing.T) {
	cfg := basicConfig()
	cfg.ObjectGroups["og_netif"] = config.ObjectGroup{Objects: []string{"MISSING-MIB::missing"}}

	jobs := scheduler.ResolveJobs(cfg, nil)
	if len(jobs) != 0 {
		t.Errorf("got %d jobs, want 0 for missing object def", len(jobs))
	}
}

func TestResolveJobs_NilConfig(t *testing.T) {
	jobs := scheduler.ResolveJobs(nil, nil)
	if jobs != nil {
		t.Errorf("expected nil for nil config, got %d jobs", len(jobs))
	}
}

func TestResolveJobs_MultipleObjects(t *testing.T) {
	cfg := basicConfig()
	cfg.ObjectGroups["og_netif"] = config.ObjectGroup{
		Objects: []string{"IF-MIB::ifEntry", "IF-MIB::ifXEntry"},
	}
	cfg.ObjectDefs["IF-MIB::ifXEntry"] = models.ObjectDefinition{
		Key:    "IF-MIB::ifXEntry",
		MIB:    "IF-MIB",
		Object: "ifXEntry",
		Index: []models.IndexDefinition{
			{Type: "Integer", OID: ".1.3.6.1.2.1.2.2.1.1", Name: "netif"},
		},
		Attributes: map[string]models.AttributeDefinition{
			"ifHCInOctets": {
				OID:    ".1.3.6.1.2.1.31.1.1.1.6",
				Name:   "netif.bytes.in",
				Syntax: "Counter64",
			},
		},
	}

	jobs := scheduler.ResolveJobs(cfg, nil)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Scheduler lifecycle tests
// ─────────────────────────────────────────────────────────────────────────────

func TestSchedulerFiresOnInterval(t *testing.T) {
	cfg := basicConfig()
	cfg.Devices["switch1"] = config.DeviceConfig{
		IP:           "10.0.0.1",
		Port:         161,
		PollInterval: 1, // 1 second
		Timeout:      500,
		Version:      "2c",
		Communities:  []string{"public"},
		DeviceGroups: []string{"group_a"},
	}

	sub := newMockSubmitter(0)
	s := scheduler.New(cfg, sub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	// Wait for at least 2 firing cycles.
	time.Sleep(2500 * time.Millisecond)
	cancel()
	s.Stop()

	count := sub.count()
	// With PollInterval=1s and 2.5s elapsed, expect at least 2 firings
	// (one immediate + one at ~1s + possibly ~2s).
	if count < 2 {
		t.Errorf("expected at least 2 dispatches in 2.5s, got %d", count)
	}
}

func TestSchedulerMultipleIntervals(t *testing.T) {
	cfg := multiDeviceConfig()
	// switch1: PollInterval=1s, router1: PollInterval=2s

	sub := newMockSubmitter(0)
	s := scheduler.New(cfg, sub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	time.Sleep(2500 * time.Millisecond)
	cancel()
	s.Stop()

	jobs := sub.getJobs()
	switchCount := 0
	routerCount := 0
	for _, j := range jobs {
		switch j.Hostname {
		case "switch1":
			switchCount++
		case "router1":
			routerCount++
		}
	}

	// switch1 (1s interval): expect at least 2 firings in 2.5s.
	if switchCount < 2 {
		t.Errorf("switch1: expected ≥2 dispatches, got %d", switchCount)
	}
	// router1 (2s interval): expect at least 1 firing in 2.5s.
	if routerCount < 1 {
		t.Errorf("router1: expected ≥1 dispatch, got %d", routerCount)
	}
	// switch1 should fire more often than router1.
	if switchCount <= routerCount {
		t.Errorf("switch1 (%d) should fire more than router1 (%d)", switchCount, routerCount)
	}
}

func TestSchedulerStop(t *testing.T) {
	cfg := basicConfig()
	sub := newMockSubmitter(0)
	s := scheduler.New(cfg, sub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	// Cancel immediately.
	cancel()

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK — scheduler stopped promptly.
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop within 2s after context cancel")
	}
}

func TestSchedulerNoop(t *testing.T) {
	// Empty config — scheduler should run without panicking.
	cfg := &config.LoadedConfig{
		Devices: map[string]config.DeviceConfig{},
	}
	sub := newMockSubmitter(0)
	s := scheduler.New(cfg, sub, nil)

	if s.Entries() != 0 {
		t.Errorf("expected 0 entries, got %d", s.Entries())
	}

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	time.Sleep(100 * time.Millisecond)
	cancel()
	s.Stop()

	if sub.count() != 0 {
		t.Errorf("expected 0 dispatches for empty config, got %d", sub.count())
	}
}

func TestSchedulerReload(t *testing.T) {
	cfg := basicConfig()
	sub := newMockSubmitter(0)
	s := scheduler.New(cfg, sub, nil)

	if s.Entries() != 1 {
		t.Fatalf("expected 1 entry after init, got %d", s.Entries())
	}

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	// Let it fire once.
	time.Sleep(200 * time.Millisecond)
	initialCount := sub.count()
	if initialCount < 1 {
		t.Fatalf("expected at least 1 dispatch before reload, got %d", initialCount)
	}

	// Reload with an additional device.
	newCfg := multiDeviceConfig()
	s.Reload(newCfg)

	if s.Entries() != 2 {
		t.Errorf("expected 2 entries after reload, got %d", s.Entries())
	}

	// Let it fire with the new config.
	time.Sleep(1500 * time.Millisecond)
	cancel()
	s.Stop()

	finalCount := sub.count()
	if finalCount <= initialCount {
		t.Errorf("expected more dispatches after reload; before=%d after=%d", initialCount, finalCount)
	}

	// Verify both devices got dispatched.
	jobs := sub.getJobs()
	hasSwitch := false
	hasRouter := false
	for _, j := range jobs {
		if j.Hostname == "switch1" {
			hasSwitch = true
		}
		if j.Hostname == "router1" {
			hasRouter = true
		}
	}
	if !hasSwitch || !hasRouter {
		t.Errorf("expected both switch1 and router1 dispatched; switch=%v router=%v", hasSwitch, hasRouter)
	}
}

func TestSchedulerReload_RemoveDevice(t *testing.T) {
	cfg := multiDeviceConfig()
	sub := newMockSubmitter(0)
	s := scheduler.New(cfg, sub, nil)

	if s.Entries() != 2 {
		t.Fatalf("expected 2 entries, got %d", s.Entries())
	}

	// Reload with only one device.
	s.Reload(basicConfig())

	if s.Entries() != 1 {
		t.Errorf("expected 1 entry after reload, got %d", s.Entries())
	}
}

func TestTrySubmitBackpressure(t *testing.T) {
	cfg := basicConfig()
	// Capacity of 0 — rejects all after first.
	sub := newMockSubmitter(1)
	s := scheduler.New(cfg, sub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	// Let it fire a couple of times.
	time.Sleep(1500 * time.Millisecond)
	cancel()
	s.Stop()

	// Only 1 should have been accepted (capacity=1).
	if sub.count() != 1 {
		t.Errorf("expected exactly 1 accepted job (capacity=1), got %d", sub.count())
	}
}

func TestSchedulerEntries(t *testing.T) {
	cfg := multiDeviceConfig()
	sub := newMockSubmitter(0)
	s := scheduler.New(cfg, sub, nil)

	if got := s.Entries(); got != 2 {
		t.Errorf("Entries() = %d, want 2", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Concurrent safety test
// ─────────────────────────────────────────────────────────────────────────────

func TestSchedulerConcurrentReload(t *testing.T) {
	cfg := basicConfig()
	sub := newMockSubmitter(0)
	s := scheduler.New(cfg, sub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go s.Start(ctx)

	// Reload concurrently from multiple goroutines.
	var wg sync.WaitGroup
	var panicCount atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicCount.Add(1)
				}
			}()
			s.Reload(multiDeviceConfig())
		}()
	}
	wg.Wait()

	cancel()
	s.Stop()

	if panicCount.Load() != 0 {
		t.Errorf("concurrent Reload caused %d panics", panicCount.Load())
	}
}
