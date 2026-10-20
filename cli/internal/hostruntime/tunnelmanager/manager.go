// Package tunnelmanager owns durable tunnel reconciliation for the stable
// Paperboat host daemon. Preview leases are intentionally outside this package.
package tunnelmanager

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

func New(config Config) (*Manager, error) {
	if config.Store == nil || config.Factory == nil || !validID(config.HostID) || config.Report == nil {
		return nil, ErrInvalidConfig
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.ReconcileInterval == 0 {
		config.ReconcileInterval = 5 * time.Second
	}
	if config.ApplyTimeout == 0 {
		config.ApplyTimeout = 30 * time.Second
	}
	if config.DrainTimeout == 0 {
		config.DrainTimeout = 15 * time.Second
	}
	if config.ReconcileInterval <= 0 || config.ApplyTimeout <= 0 || config.DrainTimeout <= 0 {
		return nil, ErrInvalidConfig
	}
	return &Manager{config: config, active: make(map[string]Active), seen: make(map[string]uint64), sessionBindings: make(map[string]controlSessionBinding), networkStates: make(map[string]networkRecoveryState), retries: make(map[string]reconcileRetry), wake: make(chan struct{}, 1)}, nil
}

// RecordControlSessionBinding remembers the exact authenticated session that
// staged a config snapshot. Process generations are monotonic for one durable
// connector. Older or equal-generation/different-session observations are
// ignored so delayed stale frames cannot fence a newer carrier.
func (m *Manager) RecordControlSessionBinding(accountID, tunnelID, connectorID, sessionID string, processGeneration, generation uint64, contentHash string) {
	if m == nil || accountID == "" || tunnelID == "" || connectorID == "" || sessionID == "" || processGeneration == 0 || generation == 0 || contentHash == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if m.sessionBindings == nil {
		m.sessionBindings = make(map[string]controlSessionBinding)
	}
	previous, ok := m.sessionBindings[tunnelID]
	if ok {
		if processGeneration < previous.ProcessGeneration || (processGeneration == previous.ProcessGeneration && (accountID != previous.AccountID || connectorID != previous.ConnectorID || sessionID != previous.SessionID)) {
			return
		}
		if processGeneration == previous.ProcessGeneration && sessionID == previous.SessionID && generation < previous.Generation {
			return
		}
	}
	m.sessionBindings[tunnelID] = controlSessionBinding{AccountID: accountID, ConnectorID: connectorID, SessionID: sessionID, ProcessGeneration: processGeneration, Generation: generation, ContentHash: contentHash}
}

// Start performs synchronous crash recovery before starting the reconciliation
// loop. A corrupt/unreadable durable store is fatal; per-tunnel origin or edge
// failures are reported and retried without discarding an existing LKG.
func (m *Manager) Start(ctx context.Context) error {
	return m.start(ctx, true)
}

// StartDeferred starts the manager lifecycle and reconciliation loop without
// running the initial reconciliation inline. It is for compositions whose
// inputs are established by another concurrently starting protocol, such as a
// connector control stream that must deliver Welcome before a carrier can be
// prepared. The caller must arrange a Notify after that protocol is ready.
// Ordinary users should call Start so crash recovery remains synchronous.
func (m *Manager) StartDeferred(ctx context.Context) error {
	if m == nil || ctx == nil {
		return ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.start(ctx, false)
}

func (m *Manager) start(ctx context.Context, reconcile bool) error {
	if m == nil || ctx == nil {
		return ErrInvalidConfig
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return ErrAlreadyStarted
	}
	if m.closed {
		m.mu.Unlock()
		return ErrNotStarted
	}
	m.ctx, m.stop = context.WithCancel(context.Background())
	m.done = make(chan struct{})
	m.started = true
	m.mu.Unlock()
	if reconcile {
		if err := m.ReconcileNow(ctx); err != nil {
			m.mu.Lock()
			m.stop()
			close(m.done)
			m.mu.Unlock()
			m.opMu.Lock()
			cleanupCtx, cancel := context.WithTimeout(context.Background(), m.config.DrainTimeout)
			cleanupErr := m.closeAll(cleanupCtx)
			cancel()
			m.opMu.Unlock()
			m.mu.Lock()
			m.started = false
			m.mu.Unlock()
			return errors.Join(err, cleanupErr)
		}
	}
	go m.run()
	return nil
}

func (m *Manager) run() {
	defer close(m.done)
	ticker := time.NewTicker(m.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		scheduled := false
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			scheduled = true
		case <-m.wake:
		}
		ctx, cancel := context.WithTimeout(m.ctx, m.config.ApplyTimeout)
		_ = m.reconcile(ctx, scheduled)
		cancel()
	}
}

// Notify coalesces desired-state changes and never blocks the protocol reader.
func (m *Manager) Notify() {
	if m == nil {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// ReconcileNow reconciles every tunnel immediately, ignoring retry backoff.
func (m *Manager) ReconcileNow(ctx context.Context) error { return m.reconcile(ctx, false) }

func (m *Manager) report(tunnel hoststate.Tunnel, connector hoststate.Connector, code string, retryable bool, err error) {
	now := m.now()
	observation := Observation{TunnelID: tunnel.ID, ConnectorID: connector.ID, DesiredGeneration: tunnel.DesiredGeneration, AppliedGeneration: tunnel.AppliedGeneration, Code: code, Retryable: retryable, ObservedAt: now, Err: err}
	if retryable {
		observation.NextRetryAt = now.Add(m.config.ReconcileInterval)
	}
	m.config.Report(observation)
}

func (m *Manager) now() time.Time { return m.config.Clock().UTC() }

func localConnector(connectors []hoststate.Connector, tunnelID, hostID string) (hoststate.Connector, error) {
	var found hoststate.Connector
	for _, connector := range connectors {
		if connector.TunnelID != tunnelID || connector.HostID != hostID {
			continue
		}
		if found.ID != "" {
			return hoststate.Connector{}, fmt.Errorf("%w: duplicate connector identity", ErrConnectorUnavailable)
		}
		found = connector
	}
	if found.ID == "" {
		return hoststate.Connector{}, ErrConnectorUnavailable
	}
	return found, nil
}

func validID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index, char := range value {
		if index == 0 && !isAlphaNumeric(char) || index > 0 && !isAlphaNumeric(char) && char != '.' && char != '_' && char != ':' && char != '-' {
			return false
		}
	}
	return true
}

func isAlphaNumeric(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

// reconcileRetry is the backoff of one tunnel whose scheduled reconcile keeps failing.
type reconcileRetry struct {
	failures   int
	generation uint64
	next       time.Time
}

// failed returns the state after one more failed attempt: the retry waits one reconcile
// interval, doubling per consecutive failure up to 48 s, plus a per-tunnel spread of up to a
// quarter, so an unreachable or deleted tunnel costs the control plane about one request a minute
// instead of one per tick and tunnels that fail together do not retry together.
func (r reconcileRetry) failed(generation uint64, now time.Time, interval time.Duration, identity string) reconcileRetry {
	if r.generation != generation {
		r = reconcileRetry{generation: generation}
	}
	r.failures++
	delay := interval
	for i := 1; i < r.failures && delay < reconcileRetryDoublingCap; i++ {
		delay *= 2
	}
	delay = min(delay, reconcileRetryDoublingCap)
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(identity))
	spread := time.Duration((hash.Sum32()+uint32(r.failures*37))%25) * delay / 100
	r.next = now.Add(delay + spread)
	return r
}

// reconcileRetryDoublingCap keeps the longest retry wait, spread included, within a minute.
const reconcileRetryDoublingCap = 48 * time.Second
