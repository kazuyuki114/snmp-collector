package poller

import (
	"context"
	"log/slog"
	"sync"

	"snmp/snmp-collector/internal/noop"
	"snmp/snmp-collector/internal/decoder"
)

// ─────────────────────────────────────────────────────────────────────────────
// WorkerPool — fan-out dispatcher for PollJobs
// ─────────────────────────────────────────────────────────────────────────────

// WorkerPool fans poll jobs out to N worker goroutines and collects results
// into a shared output channel.
type WorkerPool struct {
	numWorkers int
	poller     Poller
	output     chan<- decoder.RawPollResult
	logger     *slog.Logger

	jobs chan PollJob
	wg   sync.WaitGroup
}

// NewWorkerPool creates a pool of numWorkers goroutines that execute poll jobs
// using the supplied Poller and send results to output.
func NewWorkerPool(numWorkers int, poller Poller, output chan<- decoder.RawPollResult, jobQueueSize int, logger *slog.Logger) *WorkerPool {
	if numWorkers <= 0 {
		numWorkers = 100
	}
	if jobQueueSize <= 0 {
		jobQueueSize = numWorkers * 2
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noop.Writer{}, nil))
	}
	return &WorkerPool{
		numWorkers: numWorkers,
		poller:     poller,
		output:     output,
		logger:     logger,
		jobs:       make(chan PollJob, jobQueueSize),
	}
}

// Start launches the worker goroutines. They run until ctx is cancelled or
// Stop is called.
func (w *WorkerPool) Start(ctx context.Context) {
	for i := 0; i < w.numWorkers; i++ {
		w.wg.Add(1)
		go w.worker(ctx)
	}
}

// Submit enqueues a poll job. It blocks if the internal job channel is full.
func (w *WorkerPool) Submit(job PollJob) {
	w.jobs <- job
}

// TrySubmit enqueues a poll job without blocking. Returns false if the channel
// is full, allowing the caller to drop or defer the job.
func (w *WorkerPool) TrySubmit(job PollJob) bool {
	select {
	case w.jobs <- job:
		return true
	default:
		return false
	}
}

// Stop closes the job channel and waits for all workers to drain.
func (w *WorkerPool) Stop() {
	close(w.jobs)
	w.wg.Wait()
}

// worker is the per-goroutine loop.
func (w *WorkerPool) worker(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case job, ok := <-w.jobs:
			if !ok {
				return
			}
			result, err := w.poller.Poll(ctx, job)
			if err != nil {
				w.logger.Warn("poll failed",
					"device", job.Hostname,
					"object", job.ObjectDef.Key,
					"error", err.Error(),
				)
				if len(result.Varbinds) == 0 {
					continue
				}
				result.PollStatus = "error"
				result.ErrorType = classifyError(err)
				result.PollError = err.Error()
			} else {
				result.PollStatus = "success"
			}
			select {
			case w.output <- result:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
