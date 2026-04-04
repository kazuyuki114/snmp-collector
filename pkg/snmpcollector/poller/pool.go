package poller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
	"snmp/snmp-collector/pkg/snmpcollector/config"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────────────

// PoolOptions configures the connection pool behaviour.
type PoolOptions struct {
	// MaxIdlePerDevice is the maximum number of idle sessions kept per device
	// (default 2). Excess sessions returned via Put are closed immediately.
	MaxIdlePerDevice int

	// IdleTimeout is how long an idle session remains in the pool before being
	// discarded. Zero means no expiry.
	IdleTimeout time.Duration

	// Dial is the function used to create new gosnmp sessions.
	// Defaults to NewSession when nil.
	Dial func(config.DeviceConfig) (*gosnmp.GoSNMP, error)
}

func (o *PoolOptions) defaults() {
	if o.MaxIdlePerDevice <= 0 {
		o.MaxIdlePerDevice = 2
	}
	if o.Dial == nil {
		o.Dial = NewSession
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Connection pool
// ─────────────────────────────────────────────────────────────────────────────

// poolEntry is a single idle connection together with the time it was returned.
type poolEntry struct {
	conn       *gosnmp.GoSNMP
	returnedAt time.Time
}

// devicePool is the per-device idle list + concurrency semaphore.
type devicePool struct {
	mu   sync.Mutex
	idle []poolEntry // LIFO stack

	// sem limits concurrent in-flight connections for this device.
	// Its capacity equals DeviceConfig.MaxConcurrentPolls.
	sem chan struct{}
}

// ConnectionPool manages gosnmp sessions keyed by device hostname.
// It enforces per-device concurrency limits and recycles idle sessions.
type ConnectionPool struct {
	opts   PoolOptions
	logger *slog.Logger

	mu    sync.RWMutex
	pools map[string]*devicePool // hostname → pool

	closed chan struct{}
}

// NewConnectionPool creates a ready-to-use pool.
func NewConnectionPool(opts PoolOptions, logger *slog.Logger) *ConnectionPool {
	opts.defaults()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	return &ConnectionPool{
		opts:   opts,
		logger: logger,
		pools:  make(map[string]*devicePool),
		closed: make(chan struct{}),
	}
}

// Get acquires a gosnmp session for the given device. It blocks if the
// per-device concurrency limit has been reached, and respects context
// cancellation.
func (p *ConnectionPool) Get(ctx context.Context, hostname string, cfg config.DeviceConfig) (*gosnmp.GoSNMP, error) {
	dp := p.getOrCreatePool(hostname, cfg.MaxConcurrentPolls)

	// Fast path: reject immediately if the pool is closed.
	select {
	case <-p.closed:
		return nil, fmt.Errorf("pool closed")
	default:
	}

	// Acquire concurrency slot.
	select {
	case dp.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.closed:
		return nil, fmt.Errorf("pool closed")
	}

	// Try to reuse an idle connection.
	if conn := p.popIdle(dp); conn != nil {
		return conn, nil
	}

	// Dial a new session.
	conn, err := p.opts.Dial(cfg)
	if err != nil {
		// Release semaphore slot on failure.
		<-dp.sem
		return nil, err
	}
	return conn, nil
}

// Put returns a connection to the idle pool for reuse. If the pool is full the
// connection is closed. Put also releases the per-device concurrency slot.
func (p *ConnectionPool) Put(hostname string, conn *gosnmp.GoSNMP) {
	dp := p.getPool(hostname)
	if dp == nil {
		// Unknown device — close and return.
		if conn.Conn != nil {
			_ = conn.Conn.Close()
		}
		return
	}
	defer func() { <-dp.sem }() // Release concurrency slot.

	dp.mu.Lock()
	defer dp.mu.Unlock()

	if len(dp.idle) >= p.opts.MaxIdlePerDevice {
		if conn.Conn != nil {
			_ = conn.Conn.Close()
		}
		return
	}
	dp.idle = append(dp.idle, poolEntry{conn: conn, returnedAt: time.Now()})
}

// Discard closes a connection and releases the per-device concurrency slot
// without putting it back into the pool. Use this when a connection is known
// to be broken.
func (p *ConnectionPool) Discard(hostname string, conn *gosnmp.GoSNMP) {
	if conn.Conn != nil {
		_ = conn.Conn.Close()
	}
	dp := p.getPool(hostname)
	if dp != nil {
		<-dp.sem
	}
}

// Close drains all idle connections and prevents new Get calls.
func (p *ConnectionPool) Close() error {
	select {
	case <-p.closed:
		return nil // Already closed.
	default:
	}
	close(p.closed)

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, dp := range p.pools {
		dp.mu.Lock()
		for _, e := range dp.idle {
			if e.conn.Conn != nil {
				_ = e.conn.Conn.Close()
			}
		}
		dp.idle = nil
		dp.mu.Unlock()
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

func (p *ConnectionPool) getOrCreatePool(hostname string, maxConcurrent int) *devicePool {
	p.mu.RLock()
	dp, ok := p.pools[hostname]
	p.mu.RUnlock()
	if ok {
		return dp
	}

	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Double-check under write lock.
	if dp, ok = p.pools[hostname]; ok {
		return dp
	}
	dp = &devicePool{
		idle: make([]poolEntry, 0, p.opts.MaxIdlePerDevice),
		sem:  make(chan struct{}, maxConcurrent),
	}
	p.pools[hostname] = dp
	return dp
}

func (p *ConnectionPool) getPool(hostname string) *devicePool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pools[hostname]
}

func (p *ConnectionPool) popIdle(dp *devicePool) *gosnmp.GoSNMP {
	dp.mu.Lock()
	defer dp.mu.Unlock()

	for len(dp.idle) > 0 {
		// Pop from the end (LIFO).
		n := len(dp.idle) - 1
		entry := dp.idle[n]
		dp.idle = dp.idle[:n]

		// Check idle timeout.
		if p.opts.IdleTimeout > 0 && time.Since(entry.returnedAt) > p.opts.IdleTimeout {
			if entry.conn.Conn != nil {
				_ = entry.conn.Conn.Close()
			}
			continue
		}
		return entry.conn
	}
	return nil
}

// noopWriter discards log output.
type noopWriter struct{}

func (noopWriter) Write(b []byte) (int, error) { return len(b), nil }
