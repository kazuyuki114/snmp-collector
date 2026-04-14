package scheduler

import (
	"container/heap"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"snmp/snmp-collector/internal/noop"
	"snmp/snmp-collector/internal/config"
	"snmp/snmp-collector/internal/poller"
)

// ─────────────────────────────────────────────────────────────────────────────
// JobSubmitter — interface for dependency injection
// ─────────────────────────────────────────────────────────────────────────────

// JobSubmitter is the subset of poller.WorkerPool consumed by the scheduler.
// Using an interface lets tests inject a mock without importing the full pool.
type JobSubmitter interface {
	Submit(poller.PollJob)
	TrySubmit(poller.PollJob) bool
}

// ─────────────────────────────────────────────────────────────────────────────
// entry + min-heap
// ─────────────────────────────────────────────────────────────────────────────

// entry tracks the next-fire time for a single device and its pre-resolved jobs.
type entry struct {
	hostname string
	interval time.Duration
	nextRun  time.Time
	jobs     []poller.PollJob
}

// entryHeap implements heap.Interface for min-heap ordering by nextRun.
// The heap invariant means entries[0] is always the next entry to fire.
type entryHeap []entry

func (h entryHeap) Len() int           { return len(h) }
func (h entryHeap) Less(i, j int) bool { return h[i].nextRun.Before(h[j].nextRun) }
func (h entryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *entryHeap) Push(x any)        { *h = append(*h, x.(entry)) }
func (h *entryHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// ─────────────────────────────────────────────────────────────────────────────
// Scheduler
// ─────────────────────────────────────────────────────────────────────────────

// Scheduler dispatches PollJob values into a JobSubmitter at each device's
// configured PollInterval.
type Scheduler struct {
	pool   JobSubmitter
	logger *slog.Logger

	mu      sync.Mutex
	entries entryHeap

	dropped atomic.Int64 // cumulative count of jobs dropped due to full queue

	done chan struct{}
}

// New creates a Scheduler. The scheduler does NOT start automatically — call
// Start to begin dispatching.
func New(cfg *config.LoadedConfig, pool JobSubmitter, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noop.Writer{}, nil))
	}
	s := &Scheduler{
		pool:   pool,
		logger: logger,
		done:   make(chan struct{}),
	}
	s.entries = s.buildEntries(cfg)
	return s
}

// Start runs the scheduling loop. It blocks until ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) {
	defer close(s.done)

	for {
		s.mu.Lock()
		if s.entries.Len() == 0 {
			s.mu.Unlock()
			// Nothing to schedule — wait for context cancellation or a Reload.
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		// Heap min is always at index 0 — no sort needed.
		next := s.entries[0].nextRun
		s.mu.Unlock()

		delay := time.Until(next)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		now := time.Now()
		s.mu.Lock()
		// Pop and re-push all entries that are due. The heap invariant keeps
		// entries[0] as the minimum, so we stop as soon as the next entry is
		// in the future. This is O(k log n) for k due entries.
		for s.entries.Len() > 0 && !s.entries[0].nextRun.After(now) {
			e := heap.Pop(&s.entries).(entry)
			s.fireEntry(&e)
			e.nextRun = now.Add(e.interval)
			heap.Push(&s.entries, e)
		}
		s.mu.Unlock()
	}
}

// Stop waits for the scheduling loop to exit. The caller must cancel the
// context passed to Start before calling Stop.
func (s *Scheduler) Stop() {
	<-s.done
}

// Reload atomically replaces the running config. New devices are polled
// immediately; removed devices stop; changed intervals take effect on the
// next cycle.
func (s *Scheduler) Reload(cfg *config.LoadedConfig) {
	h := s.buildEntries(cfg)
	s.mu.Lock()
	s.entries = h
	s.mu.Unlock()
	s.logger.Info("scheduler: config reloaded", "devices", h.Len())
}

// Entries returns the number of active entries (for monitoring / tests).
func (s *Scheduler) Entries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries.Len()
}

// DroppedJobs returns the cumulative number of jobs dropped because the worker
// pool queue was full at submission time.
func (s *Scheduler) DroppedJobs() int64 {
	return s.dropped.Load()
}

// ObjectCounts returns the number of poll objects configured per device hostname.
// Used by the DeviceAggregator to know when all results for a cycle have arrived.
func (s *Scheduler) ObjectCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int, len(s.entries))
	for _, e := range s.entries {
		counts[e.hostname] = len(e.jobs)
	}
	return counts
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildEntries resolves the config hierarchy and creates a min-heap of entries,
// one per device, each scheduled to fire immediately.
func (s *Scheduler) buildEntries(cfg *config.LoadedConfig) entryHeap {
	allJobs := ResolveJobs(cfg, s.logger)
	byHost := jobsByHostname(allJobs)

	now := time.Now()
	h := make(entryHeap, 0, len(byHost))
	for hostname, jobs := range byHost {
		if len(jobs) == 0 {
			continue
		}
		interval := time.Duration(jobs[0].DeviceConfig.PollInterval) * time.Second
		if interval <= 0 {
			interval = 60 * time.Second
		}
		h = append(h, entry{
			hostname: hostname,
			interval: interval,
			nextRun:  now, // Poll immediately on start / reload.
			jobs:     jobs,
		})
	}
	heap.Init(&h)
	return h
}

// fireEntry dispatches all jobs for one entry using TrySubmit (non-blocking).
// Dropped jobs are counted in s.dropped.
func (s *Scheduler) fireEntry(e *entry) {
	for _, job := range e.jobs {
		if !s.pool.TrySubmit(job) {
			s.dropped.Add(1)
			s.logger.Warn("scheduler: job queue full, dropping job",
				"hostname", e.hostname,
				"object", job.ObjectDef.Key,
				"total_dropped", s.dropped.Load(),
			)
		}
	}
	s.logger.Debug("scheduler: fired jobs",
		"hostname", e.hostname,
		"count", len(e.jobs),
	)
}
