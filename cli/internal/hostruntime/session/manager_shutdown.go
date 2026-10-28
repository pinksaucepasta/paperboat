package session

import (
	"context"
	"errors"
	"sort"
)

// BeginUpdate atomically excludes new PTY creation and restart while checking
// that the stable host owns no live PTY process. The caller persists ownership.
func (m *Manager) BeginUpdate(transactionID string) error {
	if m == nil || transactionID == "" {
		return ErrInvalidSession
	}
	m.admission.Lock()
	defer m.admission.Unlock()
	if m.updateTransaction != "" {
		if m.updateTransaction == transactionID {
			return nil
		}
		return ErrUpdateInProgress
	}
	m.mu.RLock()
	sessions := make([]*managedSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	stopping := m.stopping
	m.mu.RUnlock()
	if stopping {
		return ErrManagerStopped
	}
	for _, session := range sessions {
		session.opMu.Lock()
		live := session.process != nil
		session.opMu.Unlock()
		if live {
			return ErrUpdateBusy
		}
	}
	m.updateTransaction = transactionID
	return nil
}

func (m *Manager) EndUpdate(transactionID string) error {
	if m == nil || transactionID == "" {
		return ErrInvalidSession
	}
	m.admission.Lock()
	defer m.admission.Unlock()
	if m.updateTransaction == "" || m.updateTransaction != transactionID {
		return ErrUpdateInProgress
	}
	m.updateTransaction = ""
	return nil
}

// Retire leaves the caller-owned store intact for a replacement owner. It
// excludes new PTY creation and restart, requires every terminal process to be
// stopped, and flushes manager-owned output persistence before returning. A
// failed flush leaves the manager fenced and its sessions available for retry.
func (m *Manager) Retire(ctx context.Context) error {
	if m == nil || ctx == nil {
		return ErrInvalidSession
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.admission.Lock()
	defer m.admission.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.RLock()
	if m.retired {
		m.mu.RUnlock()
		return nil
	}
	if m.stopping && !m.retiring {
		m.mu.RUnlock()
		return ErrManagerStopped
	}
	sessions := make([]*managedSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].id < sessions[j].id })

	locked := 0
	unlockSessions := func() {
		for index := locked - 1; index >= 0; index-- {
			sessions[index].operationMu.Unlock()
		}
		locked = 0
	}
	defer unlockSessions()
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		session.operationMu.Lock()
		locked++
	}
	for _, session := range sessions {
		session.opMu.Lock()
		state, _ := session.lifecycle.Snapshot()
		live := session.process != nil || session.liveProcess.Load() != nil
		session.opMu.Unlock()
		if live || state == Creating || state == Running || state == Restarting || state == Closing {
			return ErrUpdateBusy
		}
		if state != Exited && state != Closed {
			return ErrInvalidSession
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.stopping = true
	m.retiring = true
	m.mu.Unlock()

	var result error
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		if m.config.Store == nil {
			continue
		}
		session.persistMu.Lock()
		retryFlush := !session.persisting && session.persistErr != nil
		session.persistMu.Unlock()
		if retryFlush {
			m.startOutputPersistence(session)
		}
		err := m.stopOutputPersistenceContext(ctx, session)
		session.persistMu.Lock()
		session.persistErr = err
		session.persistMu.Unlock()
		result = errors.Join(result, err)
	}
	if result != nil {
		return result
	}

	unlockSessions()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.DetachAllAttachments(); err != nil {
		return err
	}
	for _, session := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		session.opMu.Lock()
		if session.screen != nil {
			session.screen.Dispose()
			session.screen = nil
			session.continuation.reset()
		}
		session.opMu.Unlock()
	}
	m.mu.Lock()
	m.retired = true
	m.mu.Unlock()
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.admission.Lock()
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		m.admission.Unlock()
		return nil
	}
	m.stopping = true
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	m.admission.Unlock()
	results := make(chan error, len(ids))
	for _, id := range ids {
		go func(sessionID string) {
			_, err := m.Close(ctx, sessionID)
			results <- err
		}(id)
	}
	var result error
	for range ids {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, ErrSessionRunning) {
				result = errors.Join(result, err)
			}
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		}
	}
	return result
}

// ShutdownForRecovery stops live PTYs and flushes their history while leaving
// their durable generations recoverable by the next worker process.
func (m *Manager) ShutdownForRecovery(ctx context.Context) error {
	m.mu.RLock()
	sessions := make(map[string]*managedSession, len(m.sessions))
	for id, session := range m.sessions {
		sessions[id] = session
	}
	m.mu.RUnlock()
	recoverable := make(map[string]State)
	for id, session := range sessions {
		session.opMu.Lock()
		state, _ := session.lifecycle.Snapshot()
		if state == Running || state == Restarting {
			recoverable[id] = state
		}
		session.opMu.Unlock()
	}

	result := m.Shutdown(ctx)
	if m.config.Store == nil {
		return result
	}
	restoreCtx, cancel := context.WithTimeout(context.Background(), m.config.TerminationTimeout)
	defer cancel()
	for id, original := range recoverable {
		session, err := m.get(id)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		session.operationMu.Lock()
		session.opMu.Lock()
		state, _ := session.lifecycle.Snapshot()
		if state != Closed {
			session.opMu.Unlock()
			session.operationMu.Unlock()
			result = errors.Join(result, ErrInvalidTransition)
			continue
		}
		record := session.storeRecordLocked()
		record.State = string(original)
		record.ExitCode, record.ExitSignal, record.ExitedAt = nil, "", nil
		session.opMu.Unlock()
		err = m.config.Store.UpdateSession(restoreCtx, id, string(Closed), record)
		session.operationMu.Unlock()
		result = errors.Join(result, err)
	}
	return result
}

// DetachAllAttachments releases transport participants when the active feature
// worker changes. PTYs, screen state, input decisions and replay remain owned.
func (m *Manager) DetachAllAttachments() error {
	m.mu.RLock()
	sessions := make([]*managedSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.RUnlock()
	var result error
	for _, s := range sessions {
		s.opMu.Lock()
		ids := make([]string, 0, len(s.participants))
		for id := range s.participants {
			ids = append(ids, id)
		}
		s.opMu.Unlock()
		for _, id := range ids {
			err := m.Detach(s.id, id)
			if !errors.Is(err, ErrAttachmentUnknown) && !errors.Is(err, ErrSessionUnknown) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}
