package session

import (
	"context"
	"errors"

	xterm "github.com/gitpod-io/xterm-go"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func (m *Manager) Close(ctx context.Context, sessionID string) (Snapshot, error) {
	return m.closeAtGeneration(ctx, sessionID, 0)
}

func (m *Manager) CloseAtGeneration(ctx context.Context, sessionID string, expectedGeneration uint64) (Snapshot, error) {
	if expectedGeneration == 0 {
		return Snapshot{}, ErrInvalidInput
	}
	return m.closeAtGeneration(ctx, sessionID, expectedGeneration)
}

func (m *Manager) closeAtGeneration(ctx context.Context, sessionID string, expectedGeneration uint64) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	session, err := m.get(sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if err := m.checkRetiring(); err != nil {
		return Snapshot{}, err
	}
	state, generation := session.lifecycle.Snapshot()
	if expectedGeneration != 0 && generation != expectedGeneration {
		return Snapshot{}, &StaleGenerationError{CurrentGeneration: generation}
	}
	if state == Closed {
		var persistErr error
		if m.config.Store != nil && session.persistedState != Closed {
			persistErr = m.persist(ctx, session)
		}
		if err := errors.Join(persistErr, m.clearOutputLocked(ctx, session, true)); err != nil {
			return session.snapshotLocked(), err
		}
		return session.snapshotLocked(), nil
	}
	if state == Exited {
		if err := session.lifecycle.Transition(Closing); err != nil {
			return Snapshot{}, err
		}
		if err := session.lifecycle.Transition(Closed); err != nil {
			return Snapshot{}, err
		}
		persistErr := m.persist(ctx, session)
		clearErr := m.clearOutputLocked(ctx, session, true)
		if err := errors.Join(persistErr, clearErr); err != nil {
			return Snapshot{}, err
		}
		return session.snapshotLocked(), nil
	}
	if state != Running && state != Closing || session.process == nil {
		return Snapshot{}, ErrSessionRunning
	}
	var persistenceErr error
	if state == Running {
		session.liveProcess.Store(nil)
		// Drain output before the lifecycle write, but a failed history write
		// must not prevent termination of a process the caller asked to close.
		persistenceErr = m.stopOutputPersistence(session)
		if err := session.lifecycle.Transition(Closing); err != nil {
			return Snapshot{}, errors.Join(persistenceErr, err)
		}
		persistenceErr = errors.Join(persistenceErr, m.persist(ctx, session))
	}
	terminateCtx, cancel := context.WithTimeout(ctx, m.config.TerminationTimeout)
	result, terminateErr := session.process.Terminate(terminateCtx, m.config.TerminationGrace)
	cancel()
	if onlyContextEnd(terminateErr) {
		process := session.process
		setSessionSupportReference(session, supportref.FromContext(ctx))
		go m.finishClosing(session, process)
		return session.snapshotLocked(), errors.Join(persistenceErr, terminateErr)
	}
	terminateErr = classifySessionFailure("lifecycle", terminateErr)
	if terminateErr != nil && result.ExitedAt.IsZero() {
		return session.snapshotLocked(), errors.Join(persistenceErr, terminateErr)
	}
	session.liveProcess.Store(nil)
	session.exit = &result
	session.process = nil
	if err := session.lifecycle.Transition(Closed); err != nil {
		return session.snapshotLocked(), errors.Join(persistenceErr, terminateErr, err)
	}
	persistErr := m.persist(ctx, session)
	clearErr := m.clearOutputLocked(ctx, session, true)
	return session.snapshotLocked(), errors.Join(persistenceErr, terminateErr, persistErr, clearErr)
}

func (m *Manager) finishClosing(session *managedSession, process PTYProcess) {
	session.opMu.Lock()
	defer session.opMu.Unlock()
	state, _ := session.lifecycle.Snapshot()
	if state != Closing || session.process != process {
		return
	}
	terminateCtx, cancel := context.WithTimeout(context.Background(), m.config.TerminationTimeout)
	result, err := process.Terminate(terminateCtx, m.config.TerminationGrace)
	cancel()
	if err != nil {
		observeSessionFailure(session, "lifecycle", err)
		return
	}
	session.exit = &result
	session.process = nil
	if err := session.lifecycle.Transition(Closed); err != nil {
		observeSessionFailure(session, "lifecycle", err)
		return
	}
	persistErr := m.persist(context.Background(), session)
	clearErr := m.clearOutputLocked(context.Background(), session, true)
	if persistErr != nil || clearErr != nil {
		session.persistErr = errors.Join(session.persistErr, persistErr, clearErr)
		observeSessionFailure(session, "lifecycle", errors.Join(persistErr, clearErr))
		if m.config.Metrics != nil {
			_ = m.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
		}
	}
}

func (m *Manager) Restart(sessionID string) (Snapshot, error) {
	return m.RestartContext(context.Background(), sessionID)
}

func (m *Manager) RestartAtGeneration(sessionID string, expectedGeneration uint64) (Snapshot, error) {
	return m.RestartAtGenerationContext(context.Background(), sessionID, expectedGeneration)
}

func (m *Manager) RestartContext(ctx context.Context, sessionID string) (Snapshot, error) {
	return m.restartAtGeneration(ctx, sessionID, 0)
}

func (m *Manager) RestartAtGenerationContext(ctx context.Context, sessionID string, expectedGeneration uint64) (Snapshot, error) {
	if expectedGeneration == 0 {
		return Snapshot{}, ErrInvalidInput
	}
	return m.restartAtGeneration(ctx, sessionID, expectedGeneration)
}

func (m *Manager) restartAtGeneration(ctx context.Context, sessionID string, expectedGeneration uint64) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.admission.RLock()
	defer m.admission.RUnlock()
	if m.updateTransaction != "" {
		return Snapshot{}, ErrUpdateInProgress
	}
	m.mu.RLock()
	stopping := m.stopping
	m.mu.RUnlock()
	if stopping {
		return Snapshot{}, ErrManagerStopped
	}
	session, err := m.get(sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if err := m.checkRetiring(); err != nil {
		return Snapshot{}, err
	}
	state, generation := session.lifecycle.Snapshot()
	if expectedGeneration != 0 && generation != expectedGeneration {
		return Snapshot{}, &StaleGenerationError{CurrentGeneration: generation}
	}
	if state != Exited && state != Closed {
		return Snapshot{}, ErrSessionRunning
	}
	if m.config.PreserveStoppedHistory {
		if err := m.clearOutputLocked(context.Background(), session, true); err != nil {
			return Snapshot{}, err
		}
	}
	setSessionSupportReference(session, supportref.FromContext(ctx))
	for attachmentID, participant := range session.participants {
		if participant.Role != "owner" {
			_ = session.fanout.Detach(attachmentID)
			session.removeParticipantLocked(attachmentID)
		}
	}
	if err := session.lifecycle.Transition(Restarting); err != nil {
		return Snapshot{}, err
	}
	if err := m.persist(context.Background(), session); err != nil {
		_ = session.lifecycle.Transition(Closed)
		return Snapshot{}, err
	}
	process, err := m.launch(ctx, session.command)
	if err != nil {
		_ = session.lifecycle.Transition(Closed)
		if cleanupErr := m.persist(context.Background(), session); cleanupErr != nil {
			observeSessionFailure(session, "lifecycle", cleanupErr)
		}
		return Snapshot{}, err
	}
	if err := session.lifecycle.Transition(Running); err != nil {
		closeErr := process.CloseIO()
		observeSessionFailure(session, "lifecycle", closeErr)
		return Snapshot{}, classifySessionFailure("lifecycle", errors.Join(err, closeErr))
	}
	_, generation = session.lifecycle.Snapshot()
	session.inputs.SetGeneration(generation)
	session.screen = xterm.New(xterm.WithCols(int(session.command.Dimensions.Columns)), xterm.WithRows(int(session.command.Dimensions.Rows)), xterm.WithScrollback(browserScreenScrollbackLines))
	session.continuation.reset()
	session.process, session.exit = process, nil
	session.identification = identificationTracker{}
	session.liveProcess.Store(&liveProcess{process: process, generation: generation})
	m.startOutputPersistence(session)
	if err := m.persist(context.Background(), session); err != nil {
		terminateCtx, cancel := context.WithTimeout(context.Background(), m.config.TerminationTimeout)
		_, terminateErr := process.Terminate(terminateCtx, 0)
		cancel()
		session.liveProcess.Store(nil)
		session.process = nil
		_ = session.lifecycle.Transition(Exited)
		observeSessionFailure(session, "lifecycle", terminateErr)
		return Snapshot{}, errors.Join(err, classifySessionFailure("lifecycle", terminateErr))
	}
	go m.capture(session, process)
	return session.snapshotLocked(), nil
}

func (m *Manager) Delete(sessionID string) error {
	return m.deleteAtGeneration(sessionID, 0, true)
}

func (m *Manager) DeleteAtGeneration(sessionID string, expectedGeneration uint64) error {
	if expectedGeneration == 0 {
		return ErrInvalidInput
	}
	return m.deleteAtGeneration(sessionID, expectedGeneration, false)
}

func (m *Manager) deleteAtGeneration(sessionID string, expectedGeneration uint64, closeLive bool) (resultErr error) {
	defer func() {
		if m.config.Metrics == nil {
			return
		}
		result := "removed"
		if resultErr != nil {
			result = "failed"
		}
		_ = m.config.Metrics.Record("paperboat_runtime_cleanup_total", 1, map[string]string{"kind": "session", "result": result})
	}()
	if closeLive {
		session, err := m.get(sessionID)
		if err != nil {
			return err
		}
		session.opMu.Lock()
		state, _ := session.lifecycle.Snapshot()
		session.opMu.Unlock()
		if state != Exited && state != Closed {
			if _, err := m.Close(context.Background(), sessionID); err != nil {
				return err
			}
		}
	}
	session, err := m.get(sessionID)
	if err != nil {
		return err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if err := m.checkRetiring(); err != nil {
		return err
	}
	state, generation := session.lifecycle.Snapshot()
	if expectedGeneration != 0 && generation != expectedGeneration {
		return &StaleGenerationError{CurrentGeneration: generation}
	}
	if state != Exited && state != Closed {
		return ErrSessionRunning
	}
	if m.config.Store != nil {
		if err := m.config.Store.DeleteSession(context.Background(), sessionID); err != nil {
			return err
		}
	}
	if err := session.lifecycle.Transition(Deleted); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.sessions, sessionID)
	delete(m.names, session.name)
	m.mu.Unlock()
	return nil
}
