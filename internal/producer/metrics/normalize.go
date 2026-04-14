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

	// Reset is true when the counter dropped in a way that indicates a device
	// reboot or SNMP agent restart rather than a natural rollover. When Reset
	// is true, Valid is also true and Delta is 0. Callers should suppress or
	// zero-fill the metric to avoid false spikes in alerting systems.
	Reset bool
}

// Delta records the current counter value and, if a previous sample exists,
// returns the delta and elapsed time. On first observation it stores the value
// and returns Valid=false.
//
// wrap is the rollover boundary:
//   - Pass maxCounter32 (^uint32(0) = 4294967295) for Counter32 attributes.
//   - Pass maxCounter64 (^uint64(0)) for Counter64 attributes.
//
// Wrap vs reset detection: if current < previous the function decides whether
// the decrease is a natural counter rollover or a device/agent reset:
//   - Counter64 (wrap == ^uint64(0)): always treated as a reset — Counter64
//     would take ~46 years to wrap at 100 Gbps sustained throughput.
//   - Counter32 (wrap == ^uint32(0)): if the drop (prev−current) exceeds half
//     the counter range (wrap/2), the previous value was in the upper half and
//     a natural rollover is plausible. Otherwise it is treated as a reset.
//
// On a reset the state is reseeded with current and DeltaResult.Reset=true is
// returned so callers can suppress the sample and avoid false alert spikes.
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
	} else if isCounterReset(prev.Value, current, wrap) {
		// Counter reset detected (device reboot / agent restart). Emit delta=0
		// so downstream consumers see no false spike. State is already seeded
		// with current above, so the next interval will compute correctly.
		return DeltaResult{Delta: 0, Elapsed: elapsed, Valid: true, Reset: true}
	} else {
		// Natural counter rollover: add distance to the wrap boundary plus current.
		delta = (wrap - prev.Value) + current + 1
	}

	return DeltaResult{
		Delta:   delta,
		Elapsed: elapsed,
		Valid:   true,
	}
}

// isCounterReset returns true when a decrease from prev to current should be
// interpreted as a device/agent counter reset rather than a natural rollover.
//
// For Counter64 (wrap == ^uint64(0)): any decrease is a reset — natural
// Counter64 wraps are not observed in practice.
// For Counter32 (wrap == uint64(^uint32(0))): a natural rollover produces a
// large apparent drop because prev must have been near the counter maximum.
// If the drop exceeds half the counter range the rollover is plausible;
// a smaller drop indicates a reset.
func isCounterReset(prev, current, wrap uint64) bool {
	if wrap == ^uint64(0) {
		// Counter64: always a reset on any decrease.
		return true
	}
	// Counter32: reset when the drop is ≤ half the counter range.
	// A genuine wrap requires prev to be in the upper half of the range,
	// producing a drop larger than wrap/2.
	drop := prev - current
	return drop <= wrap/2
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
