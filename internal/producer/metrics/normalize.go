package metrics

import (
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Counter delta state
// ─────────────────────────────────────────────────────────────────────────────

// CounterKey uniquely identifies a counter observation within the collector.
// It combines device identity, metric name, and table instance so that the
// counter state is isolated per object row.
type CounterKey struct {
	Device    string // Device hostname
	Attribute string // Metric attribute name, e.g. "netif.bytes.in"
	Instance  string // Table row index, e.g. "1"
}

// counterEntry holds the previously observed value and the time it was recorded.
type counterEntry struct {
	Value  uint64
	SeenAt time.Time
}

// CounterState tracks the last known value for every observed counter so that
// the producer can compute per-interval deltas. It is safe for concurrent use.
//
// Counter32 wraps at 2^32 − 1 (~4.29 × 10⁹).
// Counter64 wraps at 2^64 − 1 (~1.84 × 10¹⁹).
// The wrap threshold passed to Delta controls which rollover behaviour applies.
type CounterState struct {
	mu      sync.Mutex
	entries map[CounterKey]counterEntry
}

// NewCounterState creates a ready-to-use CounterState.
func NewCounterState() *CounterState {
	return &CounterState{
		entries: make(map[CounterKey]counterEntry),
	}
}

// DeltaResult is returned by Delta. Both fields are meaningful only when Valid
// is true.
type DeltaResult struct {
	// Delta is the computed increase in counter value since the last sample.
	// It is always ≥ 0; counter wraps are accounted for.
	Delta uint64

	// Elapsed is the time between the previous sample and this sample.
	// Callers use this to compute rates: Rate = Delta / Elapsed.Seconds()
	Elapsed time.Duration

	// Valid is false on the first observation of a key (no previous sample),
	// or if the timestamps are equal (division by zero guard).
	Valid bool
}

// Delta records the current counter value and, if a previous sample exists,
// returns the delta and elapsed time. On first observation it stores the value
// and returns Valid=false.
//
// wrap is the rollover boundary:
//   - Pass maxCounter32 (^uint32(0) = 4294967295) for Counter32 attributes.
//   - Pass maxCounter64 (^uint64(0)) for Counter64 attributes.
//
// Wrap detection: if current < previous, it is assumed the counter rolled over
// once. Multiple wraps within a single interval are not handled (would require
// extreme counter rates and very long polling intervals).
func (s *CounterState) Delta(key CounterKey, current uint64, now time.Time, wrap uint64) DeltaResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, exists := s.entries[key]
	s.entries[key] = counterEntry{Value: current, SeenAt: now}

	if !exists {
		return DeltaResult{Valid: false}
	}

	elapsed := now.Sub(prev.SeenAt)
	if elapsed <= 0 {
		return DeltaResult{Valid: false}
	}

	var delta uint64
	if current >= prev.Value {
		delta = current - prev.Value
	} else {
		// Counter wrapped once. Add the distance to wrap boundary plus current.
		delta = (wrap - prev.Value) + current + 1
	}

	return DeltaResult{
		Delta:   delta,
		Elapsed: elapsed,
		Valid:   true,
	}
}

// Remove deletes all stored state for the given key. Call this when a device is
// removed from the inventory to avoid stale state accumulating indefinitely.
func (s *CounterState) Remove(key CounterKey) {
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// Purge removes all counter entries whose last observation is older than maxAge.
// Call this on a slow timer (e.g. every 10× poll interval) to reclaim memory for
// devices that have gone away or had their object definitions changed.
func (s *CounterState) Purge(maxAge time.Duration, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := now.Add(-maxAge)
	removed := 0
	for k, e := range s.entries {
		if e.SeenAt.Before(cutoff) {
			delete(s.entries, k)
			removed++
		}
	}
	return removed
}

// ─────────────────────────────────────────────────────────────────────────────
// Syntax classification helpers used by poll.go
// ─────────────────────────────────────────────────────────────────────────────

// IsCounterSyntax returns true when the syntax represents a monotonically
// increasing counter that benefits from delta calculation.
func IsCounterSyntax(syntax string) bool {
	switch syntax {
	case "Counter32", "Counter64":
		return true
	default:
		return false
	}
}

// WrapForSyntax returns the rollover boundary for the given syntax.
// Counter32 wraps at the uint32 max; everything else uses uint64 max.
func WrapForSyntax(syntax string) uint64 {
	if syntax == "Counter32" {
		return uint64(^uint32(0)) // 4294967295
	}
	return ^uint64(0)
}

// syntaxPriority returns a sortable priority for a syntax string.
// Higher value = preferred when two attributes have the same output metric name
// for the same instance (i.e. override resolution by precision).
// Counter64 > Counter32, BandwidthMBits > Gauge32, etc.
func syntaxPriority(syntax string) int {
	switch syntax {
	case "Counter64":
		return 20
	case "Counter32":
		return 10
	case "BandwidthGBits":
		return 14
	case "BandwidthMBits":
		return 13
	case "BandwidthKBits":
		return 12
	case "BandwidthBits", "Gauge32":
		return 11
	default:
		return 0
	}
}
