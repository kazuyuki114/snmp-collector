// Package ha implements Active/Standby High Availability for the SNMP collector.
//
// # Topology
//
//	Primary (DC, preferred)  ←── POST /demote ──→  Standby (DR)
//	      ↑ Active by default                            ↑ polls GET /health
//
// # State machine
//
// Primary: on startup it sends POST /demote to its peer (best-effort), then
// unconditionally transitions to Active. If it crashes and the process
// restarts, it repeats this sequence, reclaiming Active from a DR that may
// have promoted itself during the outage.
//
// Standby: starts Idle. Polls peer GET /health every HealthCheckInterval.
// After FailoverThreshold consecutive failures it promotes itself to Active.
// When the Primary comes back it sends POST /demote; the handler transitions
// back to Standby and fires OnStopPolling before returning 200 OK, so the
// Primary can be certain polling has stopped before it begins its own.
//
// # Wiring example
//
//	mgr := ha.New(cfg, logger)
//	mgr.OnStartPolling(func() { app.Start(ctx) })
//	mgr.OnStopPolling(func()  { app.Stop() })
//	mgr.Start(rootCtx)
//	defer mgr.Stop()
//
//	// Mount the demote endpoint on the shared HTTP server.
//	healthSrv.Handle("/demote", mgr.DemoteHandler())
package ha

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Public types
// ─────────────────────────────────────────────────────────────────────────────

// Role is the static, deployment-time role of this node.
type Role string

const (
	// RolePrimary is the preferred DC node. It preempts the standby on
	// every startup by sending POST /demote before claiming Active.
	RolePrimary Role = "primary"

	// RoleStandby is the DR node. It monitors the primary's /health endpoint
	// and promotes itself to Active after FailoverThreshold consecutive failures.
	RoleStandby Role = "standby"
)

// State is the current runtime HA state of this node.
type State string

const (
	// StateActive means this node is performing SNMP polling.
	StateActive State = "active"

	// StateStandby means this node is idle; polling is suspended.
	StateStandby State = "standby"
)

// ─────────────────────────────────────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────────────────────────────────────

// Config controls Manager behaviour. Zero values fall back to the documented
// defaults applied by withDefaults.
type Config struct {
	// Role is the static role of this node. Required.
	Role Role

	// PeerURL is the base HTTP URL of the peer node, e.g. "http://10.0.0.2:9080".
	// No trailing slash. Required.
	PeerURL string

	// HealthCheckInterval is how often the Standby node calls GET /health on
	// the peer. Default: 5s.
	HealthCheckInterval time.Duration

	// HealthCheckTimeout is the per-request HTTP client timeout for each
	// health check call. Default: 5s.
	HealthCheckTimeout time.Duration

	// FailoverThreshold is the number of consecutive health-check failures
	// required before the Standby promotes itself to Active.
	// Default: 3.
	FailoverThreshold int

	// DemoteTimeout is the HTTP client timeout for the POST /demote call
	// that Primary sends to Standby on startup.
	//
	// This value must be long enough for the Standby's app.Stop() to fully
	// drain in-flight polls and close channels before the Primary begins
	// polling itself. Default: 30s.
	DemoteTimeout time.Duration
}

func (c *Config) withDefaults() {
	if c.HealthCheckInterval == 0 {
		c.HealthCheckInterval = 5 * time.Second
	}
	if c.HealthCheckTimeout == 0 {
		c.HealthCheckTimeout = 5 * time.Second
	}
	if c.FailoverThreshold == 0 {
		c.FailoverThreshold = 3
	}
	if c.DemoteTimeout == 0 {
		c.DemoteTimeout = 30 * time.Second
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Manager
// ─────────────────────────────────────────────────────────────────────────────

// Manager runs the HA state machine in a background goroutine.
//
// Construct with New, wire the callbacks with OnStartPolling / OnStopPolling,
// then call Start. Use DemoteHandler to obtain the http.HandlerFunc that
// handles POST /demote.
type Manager struct {
	cfg    Config
	logger *slog.Logger

	// mu protects state.
	mu    sync.RWMutex
	state State

	// Lifecycle callbacks. Set before Start, never changed after that.
	onStartPolling func()
	onStopPolling  func()

	// cancel shuts down the background goroutine when called.
	cancel context.CancelFunc

	// done is closed when the background goroutine exits.
	done chan struct{}
}

// New constructs a Manager with the given config and logger.
// Call OnStartPolling / OnStopPolling to wire control hooks, then call Start.
func New(cfg Config, logger *slog.Logger) *Manager {
	cfg.withDefaults()
	return &Manager{
		cfg:    cfg,
		logger: logger,
		done:   make(chan struct{}),
	}
}

// OnStartPolling registers fn as the callback invoked every time this node
// transitions to Active (StateActive). Must be called before Start.
//
// fn is called synchronously from within the state-transition path. It must
// not call Manager.State or any other Manager method that acquires mu to avoid
// a deadlock (mu is always released before fn is invoked, but beware of
// recursive locks if fn creates nested HA operations).
func (m *Manager) OnStartPolling(fn func()) { m.onStartPolling = fn }

// OnStopPolling registers fn as the callback invoked every time this node
// transitions to Standby (StateStandby) from Active. Must be called before
// Start. The same caveats as OnStartPolling apply.
func (m *Manager) OnStopPolling(fn func()) { m.onStopPolling = fn }

// State returns the current HA state. Safe to call from any goroutine.
func (m *Manager) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Start launches the HA background loop and returns immediately.
// ctx is the root context: when it is cancelled the background loop exits,
// equivalent to calling Stop.
func (m *Manager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)
	switch m.cfg.Role {
	case RolePrimary:
		go m.runPrimary(ctx)
	default:
		go m.runStandby(ctx)
	}
}

// Stop signals the background loop to exit and blocks until it returns.
// Safe to call if Start was never called.
func (m *Manager) Stop() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	<-m.done
}

// DemoteHandler returns an http.HandlerFunc for POST /demote.
//
// Mount this on the HTTP server before calling Start:
//
//	healthSrv.Handle("/demote", mgr.DemoteHandler())
//
// The handler is meaningful only on the Standby node; mounting it on the
// Primary is harmless. When invoked it:
//  1. Transitions this node from Active to Standby.
//  2. Calls OnStopPolling synchronously (blocks until the app stops).
//  3. Returns HTTP 200 {"status":"yielded"}.
//
// The Primary blocks on the HTTP call until this returns, so it can be
// confident polling has stopped before it begins its own cycle.
func (m *Manager) DemoteHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.logger.Info("ha: /demote received — yielding to primary")
		// transitionTo releases mu before calling onStopPolling, so no
		// deadlock even if onStopPolling acquires internal app locks.
		m.transitionTo(StateStandby)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "yielded"})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Primary role
// ─────────────────────────────────────────────────────────────────────────────

// runPrimary is the background loop for the Primary (DC) node.
//
// On every startup it attempts to demote the peer (best-effort), then claims
// Active unconditionally. If the Primary crashes and restarts it will demote
// the Standby again, reclaiming the preferred role.
func (m *Manager) runPrimary(ctx context.Context) {
	defer close(m.done)

	m.logger.Info("ha: primary node starting",
		"peer_url", m.cfg.PeerURL,
		"demote_timeout", m.cfg.DemoteTimeout,
	)

	// Best-effort preemption: tell the peer to stop polling.
	// If the peer is unreachable it is either still down (no threat) or will
	// receive /demote in a future restart cycle.
	m.demotePeer(ctx)

	// Claim Active — this node is the preferred collector.
	m.transitionTo(StateActive)

	// Primary has no further state-machine loop: it stays Active until the
	// process stops (on restart it runs through this sequence again).
	<-ctx.Done()
	m.logger.Info("ha: primary shutting down")
}

// demotePeer sends POST /demote to the peer's /demote endpoint.
// Failure is logged but not fatal.
func (m *Manager) demotePeer(ctx context.Context) {
	url := m.cfg.PeerURL + "/demote"
	m.logger.Info("ha: sending /demote to peer", "url", url)

	client := &http.Client{Timeout: m.cfg.DemoteTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		// ctx already cancelled — shutdown in progress, no need to demote.
		m.logger.Warn("ha: could not build /demote request", "error", err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		m.logger.Warn("ha: /demote call failed — peer may be down, proceeding to Active",
			"url", url,
			"error", err,
		)
		return
	}
	defer resp.Body.Close()

	m.logger.Info("ha: peer responded to /demote",
		"http_status", resp.StatusCode,
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Standby role
// ─────────────────────────────────────────────────────────────────────────────

// runStandby is the background loop for the Standby (DR) node.
//
// It starts idle and monitors the peer's /health endpoint, promoting itself
// to Active after FailoverThreshold consecutive failures. It returns to
// Standby only when the /demote handler fires (i.e. the Primary is back).
func (m *Manager) runStandby(ctx context.Context) {
	defer close(m.done)

	m.logger.Info("ha: standby node starting",
		"peer_url", m.cfg.PeerURL,
		"check_interval", m.cfg.HealthCheckInterval,
		"failover_threshold", m.cfg.FailoverThreshold,
	)

	// DR begins idle.
	m.transitionTo(StateStandby)

	client := &http.Client{Timeout: m.cfg.HealthCheckTimeout}
	ticker := time.NewTicker(m.cfg.HealthCheckInterval)
	defer ticker.Stop()

	consecutiveFails := 0

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("ha: standby shutting down")
			return
		case <-ticker.C:
			m.healthCheckCycle(ctx, client, &consecutiveFails)
		}
	}
}

// healthCheckCycle performs one GET /health call against the peer.
// It updates consecutiveFails and promotes to Active when the threshold is met.
//
// Logging strategy:
//   - StateStandby: failures are WARN (they drive failover decisions).
//   - StateActive:  failures are Debug (we are already covering for the peer;
//     no action will be taken from here — failback is triggered by the Primary
//     sending POST /demote on its restart, not by this loop).
//
// Flap-prevention:
//   - Promotion only fires when state == StateStandby.
//   - When already Active and the threshold is reached, the counter is reset so
//     it does not grow without bound and spam the logs.
func (m *Manager) healthCheckCycle(ctx context.Context, client *http.Client, fails *int) {
	url := m.cfg.PeerURL + "/health"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// ctx was cancelled between the ticker fire and here; the outer
		// select will catch the Done case on the next iteration.
		return
	}

	currentState := m.State()

	resp, err := client.Do(req)
	if err != nil {
		*fails++
		if currentState == StateStandby {
			m.logger.Warn("ha: peer health check failed",
				"consecutive_failures", *fails,
				"threshold", m.cfg.FailoverThreshold,
				"error", err,
			)
		} else {
			m.logger.Debug("ha: peer still unreachable (already active, awaiting /demote from primary)",
				"consecutive_failures", *fails,
				"error", err,
			)
		}
	} else {
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			if *fails > 0 {
				m.logger.Info("ha: peer health check recovered",
					"previous_consecutive_failures", *fails,
					"state", string(currentState),
				)
				*fails = 0
			}
			// Peer is healthy. If we are already Active (peer recovered but
			// has not yet sent /demote), keep polling — the Primary initiates
			// the handoff via POST /demote on its restart.
			return
		}

		// Any non-200 status counts as a failure.
		*fails++
		if currentState == StateStandby {
			m.logger.Warn("ha: peer health check returned non-200",
				"http_status", resp.StatusCode,
				"consecutive_failures", *fails,
				"threshold", m.cfg.FailoverThreshold,
			)
		} else {
			m.logger.Debug("ha: peer returned non-200 (already active, awaiting /demote from primary)",
				"http_status", resp.StatusCode,
				"consecutive_failures", *fails,
			)
		}
	}

	if *fails >= m.cfg.FailoverThreshold {
		if currentState == StateStandby {
			m.logger.Warn("ha: failover threshold reached — promoting to active",
				"consecutive_failures", *fails,
			)
			*fails = 0
			m.transitionTo(StateActive)
		} else {
			// Already Active. Reset the counter so it never grows without
			// bound. Failback happens via POST /demote from the Primary.
			*fails = 0
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// State transitions
// ─────────────────────────────────────────────────────────────────────────────

// transitionTo atomically sets the new state and fires the appropriate callback.
//
// The mutex is always released *before* any callback is invoked. This is
// critical: onStartPolling and onStopPolling may call app.Start / app.Stop
// which hold their own internal locks. Holding mu across those calls would
// create a lock-order inversion.
func (m *Manager) transitionTo(next State) {
	m.mu.Lock()
	prev := m.state
	if prev == next {
		m.mu.Unlock()
		return
	}
	m.state = next
	m.mu.Unlock() // ← released before callbacks

	m.logger.Info("ha: state transition",
		"from", string(prev),
		"to", string(next),
	)

	switch next {
	case StateActive:
		if m.onStartPolling != nil {
			m.onStartPolling()
		}
	case StateStandby:
		// Only fire stopPolling when transitioning *from* Active, so that the
		// initial Standby → Standby no-op at startup does not trigger a stop.
		if prev == StateActive && m.onStopPolling != nil {
			m.onStopPolling()
		}
	}
}
