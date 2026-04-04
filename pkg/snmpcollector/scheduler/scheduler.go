package scheduler

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"snmp/snmp-collector/pkg/snmpcollector/config"
	"snmp/snmp-collector/pkg/snmpcollector/poller"
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
// Scheduler
// ─────────────────────────────────────────────────────────────────────────────

// entry tracks the next-fire time for a single device and its pre-resolved jobs.
type entry struct {
	hostname string
	interval time.Duration
	nextRun  time.Time
	jobs     []poller.PollJob
}

// Scheduler dispatches PollJob values into a JobSubmitter at each device's
// configured PollInterval.
type Scheduler struct {
	pool   JobSubmitter
	logger *slog.Logger

	mu      sync.Mutex
	entries []entry

	done chan struct{}
}

// New creates a Scheduler. The scheduler does NOT start automatically — call
// Start to begin dispatching.
func New(cfg *config.LoadedConfig, pool JobSubmitter, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
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
		if len(s.entries) == 0 {
			s.mu.Unlock()
			// Nothing to schedule — wait for context cancellation or a Reload.
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}

		// Sort by next run time.
		sort.Slice(s.entries, func(i, j int) bool {
			return s.entries[i].nextRun.Before(s.entries[j].nextRun)
		})
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
		for i := range s.entries {
			if s.entries[i].nextRun.After(now) {
				break
			}
			s.fireEntry(&s.entries[i])
			s.entries[i].nextRun = now.Add(s.entries[i].interval)
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
	newEntries := s.buildEntries(cfg)
	s.mu.Lock()
	s.entries = newEntries
	s.mu.Unlock()
	s.logger.Info("scheduler: config reloaded", "devices", len(newEntries))
}

// Entries returns the number of active entries (for monitoring / tests).
func (s *Scheduler) Entries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// buildEntries resolves the config hierarchy and creates one entry per device.
func (s *Scheduler) buildEntries(cfg *config.LoadedConfig) []entry {
	allJobs := ResolveJobs(cfg, s.logger)
	byHost := jobsByHostname(allJobs)

	now := time.Now()
	entries := make([]entry, 0, len(byHost))
	for hostname, jobs := range byHost {
		if len(jobs) == 0 {
			continue
		}
		interval := time.Duration(jobs[0].DeviceConfig.PollInterval) * time.Second
		if interval <= 0 {
			interval = 60 * time.Second
		}
		entries = append(entries, entry{
			hostname: hostname,
			interval: interval,
			nextRun:  now, // Poll immediately on start / reload.
			jobs:     jobs,
		})
	}
	return entries
}

// fireEntry dispatches all jobs for one entry using TrySubmit (non-blocking).
func (s *Scheduler) fireEntry(e *entry) {
	for _, job := range e.jobs {
		if !s.pool.TrySubmit(job) {
			s.logger.Warn("scheduler: job queue full, dropping job",
				"hostname", e.hostname,
				"object", job.ObjectDef.Key,
			)
		}
	}
	s.logger.Debug("scheduler: fired jobs",
		"hostname", e.hostname,
		"count", len(e.jobs),
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// noopWriter — discard log output when no logger is provided
// ─────────────────────────────────────────────────────────────────────────────

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
