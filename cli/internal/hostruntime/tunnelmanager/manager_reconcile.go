package tunnelmanager

import (
	"context"
	"errors"
	"sort"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

// reconcile applies desired state. A scheduled pass (the periodic tick) skips tunnels still in
// their failure backoff for the same desired generation; explicit and notified passes never do.
func (m *Manager) reconcile(ctx context.Context, scheduled bool) error {
	if m == nil || ctx == nil {
		return ErrInvalidConfig
	}
	m.mu.RLock()
	started := m.started && !m.closed
	m.mu.RUnlock()
	if !started {
		return ErrNotStarted
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	managerCtx := m.ctx
	m.mu.RUnlock()
	linkedCtx, cancel := context.WithCancel(ctx)
	stopLink := context.AfterFunc(managerCtx, cancel)
	defer func() {
		stopLink()
		cancel()
	}()
	ctx = linkedCtx
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.RLock()
	started = m.started && !m.closed
	m.mu.RUnlock()
	if !started {
		return ErrNotStarted
	}
	var refreshErr error
	if m.config.Refresh != nil {
		refreshErr = m.config.Refresh(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	state, _, err := m.config.Store.Snapshot()
	if err != nil {
		return err
	}
	tunnels := append([]hoststate.Tunnel(nil), state.Tunnels...)
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i].ID < tunnels[j].ID })
	wanted := make(map[string]struct{}, len(tunnels))
	for _, tunnel := range tunnels {
		wanted[tunnel.ID] = struct{}{}
		if refreshErr != nil {
			m.report(tunnel, hoststate.Connector{}, CodeControlUnavailable, true, refreshErr)
		}
		connector, connectorErr := localConnector(state.Connectors, tunnel.ID, m.config.HostID)
		if connectorErr != nil {
			m.report(tunnel, hoststate.Connector{}, CodeConnectorUnavailable, true, connectorErr)
			continue
		}
		if retry, waiting := m.retries[tunnel.ID]; scheduled && waiting && retry.generation == tunnel.DesiredGeneration && m.now().Before(retry.next) {
			continue
		}
		if err := m.reconcileTunnel(ctx, tunnel, connector); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if errors.Is(err, ErrConnectorUnavailable) {
				m.retries[tunnel.ID] = m.retries[tunnel.ID].failed(tunnel.DesiredGeneration, m.now(), m.config.ReconcileInterval)
			}
			// Runtime, origin, and generation failures are already reported for
			// this tunnel. Untyped failures are global and must not be swallowed.
			if !errors.Is(err, ErrOriginUnavailable) && !errors.Is(err, ErrConnectorUnavailable) && !errors.Is(err, ErrGenerationConflict) {
				return err
			}
		} else {
			delete(m.retries, tunnel.ID)
		}
	}
	for id := range m.retries {
		if _, ok := wanted[id]; !ok {
			delete(m.retries, id)
		}
	}
	for tunnelID, active := range m.snapshotActive() {
		if _, ok := wanted[tunnelID]; ok {
			continue
		}
		m.removeAndDrain(ctx, tunnelID, active)
		m.config.Report(Observation{TunnelID: tunnelID, ConnectorID: active.ConnectorID(), AppliedGeneration: active.Generation(), Code: CodeStopped, ObservedAt: m.now()})
	}
	return nil
}

func (m *Manager) reconcileTunnel(ctx context.Context, tunnel hoststate.Tunnel, connector hoststate.Connector) error {
	decoded, err := hoststate.ParseTunnelConfigSnapshot(tunnel.DesiredSnapshot.Payload, tunnel.ID, tunnel.DesiredGeneration)
	if err != nil || decoded.DesiredState != tunnel.DesiredState {
		if err == nil {
			err = ErrGenerationConflict
		}
		m.report(tunnel, connector, CodeApplyFailed, false, err)
		return err
	}
	if !m.acceptGeneration(tunnel.ID, tunnel.DesiredGeneration) {
		m.report(tunnel, connector, CodeApplyFailed, false, ErrGenerationConflict)
		return ErrGenerationConflict
	}
	if tunnel.DesiredState == "paused" {
		return m.pause(ctx, tunnel, connector)
	}
	current := m.activeFor(tunnel.ID)
	if current != nil && current.Generation() > tunnel.DesiredGeneration {
		m.report(tunnel, connector, CodeApplyFailed, false, ErrGenerationConflict)
		return ErrGenerationConflict
	}
	if current != nil && current.ConnectorID() == connector.ID && current.Generation() == tunnel.DesiredGeneration && current.ContentHash() == tunnel.DesiredSnapshot.ContentHash {
		if m.activeMatchesControlSession(tunnel.ID, current) {
			return nil
		}
		// The durable config is unchanged, but the authenticated control
		// session changed. Reattach the same config with the new session
		// identity without promoting another config/LKG generation.
		return m.apply(ctx, tunnel, connector, tunnel.DesiredSnapshot, false, false)
	}
	if current == nil && tunnel.LastKnownGood != nil {
		lkg, err := hoststate.ParseTunnelConfigSnapshot(tunnel.LastKnownGood.Payload, tunnel.ID, tunnel.LastKnownGood.Generation)
		if err != nil {
			m.report(tunnel, connector, CodeApplyFailed, false, err)
			return err
		}
		// A paused LKG is durable evidence that traffic was intentionally
		// disabled. It must never be reattached during restart recovery.
		if lkg.DesiredState == "active" {
			if err := m.apply(ctx, tunnel, connector, *tunnel.LastKnownGood, true, false); err != nil {
				return err
			}
		}
		if lkg.DesiredState != "active" && lkg.DesiredState != "paused" {
			return ErrGenerationConflict
		}
		if lkg.DesiredState == "active" {
			current = m.activeFor(tunnel.ID)
		}
	}
	if current != nil && current.ConnectorID() == connector.ID && current.Generation() == tunnel.DesiredGeneration && current.ContentHash() == tunnel.DesiredSnapshot.ContentHash {
		if m.activeMatchesControlSession(tunnel.ID, current) {
			return nil
		}
		return m.apply(ctx, tunnel, connector, tunnel.DesiredSnapshot, false, false)
	}
	return m.apply(ctx, tunnel, connector, tunnel.DesiredSnapshot, false, true)
}

func (m *Manager) pause(ctx context.Context, tunnel hoststate.Tunnel, connector hoststate.Connector) error {
	changed := false
	if current := m.activeFor(tunnel.ID); current != nil {
		m.removeAndDrain(ctx, tunnel.ID, current)
		changed = true
	}
	if tunnel.AppliedGeneration != tunnel.DesiredGeneration || tunnel.LastKnownGood == nil || tunnel.LastKnownGood.ContentHash != tunnel.DesiredSnapshot.ContentHash {
		if err := m.promote(ctx, tunnel, tunnel.DesiredSnapshot, connector); err != nil {
			m.report(tunnel, connector, CodePersistenceConflict, true, err)
			return err
		}
		changed = true
	}
	if changed {
		m.report(tunnel, connector, CodePaused, false, nil)
	}
	return nil
}

func (m *Manager) apply(ctx context.Context, tunnel hoststate.Tunnel, connector hoststate.Connector, snapshot hoststate.ConfigSnapshot, recovery, promote bool) error {
	decoded, err := hoststate.ParseTunnelConfigSnapshot(snapshot.Payload, tunnel.ID, snapshot.Generation)
	if err != nil {
		m.report(tunnel, connector, CodeApplyFailed, false, err)
		return err
	}
	candidate, err := m.config.Factory.Prepare(ctx, ApplyRequest{Tunnel: tunnel, Connector: connector, Snapshot: snapshot, Decoded: decoded, Recovery: recovery})
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return err
		case errors.Is(err, ErrConnectorUnavailable):
			m.report(tunnel, connector, CodeConnectorUnavailable, true, err)
		case errors.Is(err, ErrGenerationConflict):
			m.report(tunnel, connector, CodeApplyFailed, false, err)
		default:
			m.report(tunnel, connector, CodeApplyFailed, false, err)
		}
		return err
	}
	if candidate == nil {
		err = ErrConnectorUnavailable
		m.report(tunnel, connector, CodeConnectorUnavailable, true, err)
		return err
	}
	abort := func() {
		abortCtx, cancel := context.WithTimeout(context.Background(), m.config.DrainTimeout)
		defer cancel()
		_ = candidate.Abort(abortCtx)
	}
	probe, err := candidate.ProbeOrigins(ctx)
	if ctx.Err() != nil {
		abort()
		return ctx.Err()
	}
	if err != nil || !probe.Ready {
		if recovery {
			// A previously applied generation may reattach while the local
			// origin is down. This preserves endpoint and carrier identity while
			// accurately reporting degraded origin health.
			m.report(tunnel, connector, CodeOriginUnavailable, true, errors.Join(ErrOriginUnavailable, err))
		} else {
			abort()
			m.report(tunnel, connector, CodeOriginUnavailable, true, errors.Join(ErrOriginUnavailable, err))
			return errors.Join(ErrOriginUnavailable, err)
		}
	}
	// Shutdown cancels the manager context before waiting for this gate. Once
	// the gate is held, activation and optional durable promotion are one
	// lifecycle-linearized unit and cannot begin after stopped state publishes.
	m.activationMu.Lock()
	m.mu.RLock()
	running := m.started && !m.closed
	m.mu.RUnlock()
	if !running || ctx.Err() != nil {
		m.activationMu.Unlock()
		abort()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrNotStarted
	}
	active, err := candidate.Activate(ctx)
	if err != nil || active == nil || active.TunnelID() != tunnel.ID || active.ConnectorID() != connector.ID || active.Generation() != snapshot.Generation || active.ContentHash() != snapshot.ContentHash || !m.activeMatchesControlSession(tunnel.ID, active) {
		m.activationMu.Unlock()
		if active != nil {
			_ = m.closeActive(context.Background(), active)
		}
		abort()
		if err == nil {
			err = ErrGenerationConflict
		}
		m.report(tunnel, connector, CodeApplyFailed, true, err)
		return err
	}
	if err := ctx.Err(); err != nil {
		m.activationMu.Unlock()
		_ = m.closeActive(context.Background(), active)
		return err
	}
	if expected, ok := ctx.Value(networkGenerationContextKey{}).(networkGenerationContext); ok && !m.networkGenerationCurrent(expected.TunnelID, expected.Generation) {
		m.activationMu.Unlock()
		_ = m.closeActive(context.Background(), active)
		return ErrGenerationConflict
	}
	if promote {
		if err := m.promote(ctx, tunnel, snapshot, connector); err != nil {
			m.activationMu.Unlock()
			_ = m.closeActive(context.Background(), active)
			m.report(tunnel, connector, CodePersistenceConflict, true, err)
			return err
		}
	}
	m.activationMu.Unlock()
	old, ok, swapErr := m.swapActiveIfRunning(tunnel.ID, active, ctx)
	if !ok {
		_ = m.closeActive(context.Background(), active)
		if swapErr != nil {
			return swapErr
		}
		return ErrNotStarted
	}
	if old != nil && old != active {
		m.purgeRemovedInspectorRoutes(activeRouteIDs(old), activeRouteIDs(active))
		m.drain(ctx, old)
		_ = m.closeActive(context.Background(), old)
	}
	code := CodeReady
	if recovery {
		code = CodeReattached
	} else if _, ok := ctx.Value(networkGenerationContextKey{}).(networkGenerationContext); ok {
		code = CodeNetworkReattached
	}
	m.report(tunnel, connector, code, false, nil)
	return nil
}

func (m *Manager) promote(ctx context.Context, expectedTunnel hoststate.Tunnel, snapshot hoststate.ConfigSnapshot, expectedConnector hoststate.Connector) error {
	for attempt := 0; attempt < 4; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, revision, err := m.config.Store.Snapshot()
		if err != nil {
			return err
		}
		found := false
		for index := range state.Tunnels {
			tunnel := &state.Tunnels[index]
			if tunnel.ID != snapshot.TunnelID {
				continue
			}
			found = true
			if tunnel.StableEndpointID != expectedTunnel.StableEndpointID || tunnel.DesiredState != expectedTunnel.DesiredState || tunnel.DesiredGeneration != snapshot.Generation || tunnel.DesiredSnapshot.ContentHash != snapshot.ContentHash {
				return ErrGenerationConflict
			}
			lkg := snapshot
			tunnel.AppliedGeneration = snapshot.Generation
			tunnel.LastKnownGood = &lkg
			tunnel.UpdatedAt = m.now()
		}
		if !found {
			return ErrGenerationConflict
		}
		connectorFound := false
		for index := range state.Connectors {
			if state.Connectors[index].ID == expectedConnector.ID && state.Connectors[index].TunnelID == snapshot.TunnelID && state.Connectors[index].HostID == expectedConnector.HostID {
				connectorFound = true
				state.Connectors[index].LastAppliedGeneration = snapshot.Generation
			}
		}
		if !connectorFound {
			return ErrGenerationConflict
		}
		// A crash-safe local commit is intentionally non-cancelable once its
		// fsync/rename sequence begins. Cancellation observed after it commits
		// prevents runtime publication; restart/reconcile safely reattaches the
		// newly durable LKG instead of attempting an unsafe rollback.
		if _, err := m.config.Store.Commit(revision, state); err == nil {
			return ctx.Err()
		} else if !errors.Is(err, hoststate.ErrConflict) {
			return err
		}
	}
	return ErrGenerationConflict
}
