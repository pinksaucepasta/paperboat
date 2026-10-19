package tunnelmanager

import (
	"context"
	"errors"
)

func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil || ctx == nil {
		return ErrInvalidConfig
	}
	m.lifecycleMu.Lock()
	m.mu.Lock()
	shutdownDone := m.shutdownDone
	initiated := false
	if shutdownDone == nil && !m.started {
		m.mu.Unlock()
		m.lifecycleMu.Unlock()
		return nil
	}
	if shutdownDone == nil {
		stop, runDone := m.stop, m.done
		shutdownDone = make(chan struct{})
		m.shutdownDone = shutdownDone
		initiated = true
		m.mu.Unlock()
		stop()
		go m.finishShutdown(runDone, shutdownDone)
	} else {
		m.mu.Unlock()
	}
	m.lifecycleMu.Unlock()
	select {
	case <-shutdownDone:
		m.mu.RLock()
		err := m.shutdownErr
		m.mu.RUnlock()
		if err == nil {
			return nil
		}
		if initiated {
			return err
		}
		return m.retryShutdown(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) retryShutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.opMu.Lock()
	err := m.closeAll(ctx)
	m.opMu.Unlock()
	m.mu.Lock()
	m.shutdownErr = err
	m.mu.Unlock()
	return err
}

func (m *Manager) finishShutdown(runDone, shutdownDone chan struct{}) {
	m.activationMu.Lock()
	m.mu.Lock()
	m.started = false
	m.closed = true
	m.mu.Unlock()
	m.activationMu.Unlock()
	<-runDone
	m.opMu.Lock()
	err := m.closeAll(context.Background())
	m.opMu.Unlock()
	m.mu.Lock()
	m.shutdownErr = err
	close(shutdownDone)
	m.mu.Unlock()
}

func (m *Manager) removeAndDrain(ctx context.Context, tunnelID string, active Active) {
	m.mu.Lock()
	removed := m.active[tunnelID] == active
	if removed {
		delete(m.active, tunnelID)
	}
	observer := m.config.ActiveObserver
	purge := m.config.InspectorPurge
	routes := activeRouteIDs(active)
	m.mu.Unlock()
	if removed && observer != nil {
		observer(ActiveChange{TunnelID: tunnelID, Previous: active})
	}
	// A removed active no longer forwards: deny retrieval/replay and purge
	// its retained captures immediately (pause, deletion, unwanted tunnel).
	if removed && purge != nil && len(routes) > 0 {
		purge(routes)
	}
	m.drain(ctx, active)
	_ = m.closeActive(context.Background(), active)
}

func (m *Manager) closeAll(ctx context.Context) error {
	var joined error
	for tunnelID, active := range m.snapshotActive() {
		m.drain(ctx, active)
		if err := m.closeActive(ctx, active); err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		m.mu.Lock()
		removed := m.active[tunnelID] == active
		if removed {
			delete(m.active, tunnelID)
		}
		observer := m.config.ActiveObserver
		m.mu.Unlock()
		if removed && observer != nil {
			observer(ActiveChange{TunnelID: tunnelID, Previous: active})
		}
	}
	return joined
}

func (m *Manager) closeActive(parent context.Context, active Active) error {
	ctx, cancel := context.WithTimeout(parent, m.config.DrainTimeout)
	defer cancel()
	return active.Close(ctx)
}

func (m *Manager) drain(parent context.Context, active Active) {
	ctx, cancel := context.WithTimeout(parent, m.config.DrainTimeout)
	defer cancel()
	_ = active.Drain(ctx)
}
