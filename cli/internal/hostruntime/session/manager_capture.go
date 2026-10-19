package session

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
)

func (m *Manager) capture(session *managedSession, process PTYProcess) {
	var readErr, deliveryErr error
	for {
		buffer := history.AcquireBuffer()
		n, err := process.Read(buffer)
		if n > 0 {
			session.opMu.Lock()
			state, _ := session.lifecycle.Snapshot()
			if state == Running || state == Closing {
				session.modes.Consume(buffer[:n])
				session.identification.Consume(buffer[:n])
				event, appendErr := session.history.AppendBuffer(1, buffer[:n])
				if appendErr == nil {
					if session.screen != nil {
						_, _ = session.screen.Write(buffer[:n])
						session.continuation.feed(buffer[:n])
					}
					_, deliveryErr = session.fanout.PublishOwned(event)
					m.queueOutputPersistence(session, event)
					event.Release()
				} else {
					deliveryErr = appendErr
				}
			} else {
				history.ReleaseBuffer(buffer)
			}
			session.opMu.Unlock()
		} else {
			history.ReleaseBuffer(buffer)
		}
		if deliveryErr != nil || err != nil {
			readErr = errors.Join(deliveryErr, err)
			break
		}
	}
	var result pty.ExitResult
	var waitErr, terminateErr error
	if deliveryErr != nil {
		// No later output can repair a sequence/order failure. Stop the owned
		// process before joining persistence instead of silently draining output.
		terminateCtx, cancel := context.WithTimeout(context.Background(), m.config.TerminationTimeout)
		result, terminateErr = process.Terminate(terminateCtx, m.config.TerminationGrace)
		cancel()
	}
	persistErr := m.stopOutputPersistence(session)
	if deliveryErr == nil {
		result, waitErr = process.Wait(context.Background())
	}
	closeErr := process.CloseIO()
	observeSessionFailure(session, "delivery", classifySessionFailure("delivery", readErr))
	observeSessionFailure(session, "command", classifySessionFailure("command", waitErr))
	lifecycleErr := errors.Join(persistErr, terminateErr, closeErr)
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if session.process != process {
		observeSessionFailure(session, "lifecycle", lifecycleErr)
		return
	}
	session.persistErr = persistErr
	if persistErr != nil && m.config.Metrics != nil {
		_ = m.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
	}
	state, _ := session.lifecycle.Snapshot()
	if deliveryErr != nil && result.ExitedAt.IsZero() {
		// Failed termination has not proved an exit. Fence input, retain the
		// process for a later Close, and reuse bounded closing recovery only for
		// a context-limited attempt.
		if state == Running {
			session.liveProcess.Store(nil)
			if err := session.lifecycle.Transition(Closing); err != nil {
				lifecycleErr = errors.Join(lifecycleErr, err)
			} else if err := m.persist(context.Background(), session); err != nil {
				session.persistErr = errors.Join(session.persistErr, err)
				lifecycleErr = errors.Join(lifecycleErr, err)
			}
		}
		observeSessionFailure(session, "lifecycle", lifecycleErr)
		if onlyContextEnd(terminateErr) {
			go m.finishClosing(session, process)
		}
		return
	}
	if state == Running {
		if err := session.lifecycle.Transition(Exited); err != nil {
			lifecycleErr = errors.Join(lifecycleErr, err)
		}
		session.exit = &result
		session.liveProcess.CompareAndSwap(session.liveProcess.Load(), nil)
		session.process = nil
		if err := m.persist(context.Background(), session); err != nil {
			session.persistErr = errors.Join(session.persistErr, err)
			lifecycleErr = errors.Join(lifecycleErr, err)
			if m.config.Metrics != nil {
				_ = m.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
			}
		}
		if !m.config.PreserveStoppedHistory {
			if clearErr := m.clearOutputLocked(context.Background(), session, true); clearErr != nil {
				session.persistErr = errors.Join(session.persistErr, clearErr)
				lifecycleErr = errors.Join(lifecycleErr, clearErr)
				if m.config.Metrics != nil {
					_ = m.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
				}
			}
		}
	}
	observeSessionFailure(session, "lifecycle", lifecycleErr)
}

func (m *Manager) startOutputPersistence(session *managedSession) {
	if m.config.Store == nil {
		return
	}
	session.persistMu.Lock()
	defer session.persistMu.Unlock()
	session.persistNotify = make(chan struct{}, 1)
	session.persistStop = make(chan struct{})
	session.persistStopClosed = false
	session.persistDone = make(chan error, 1)
	session.persistErr = nil
	go m.runOutputPersistence(session, session.persistNotify, session.persistStop, session.persistDone)
}

func (m *Manager) queueOutputPersistence(session *managedSession, event history.Event) {
	session.persistMu.Lock()
	defer session.persistMu.Unlock()
	if session.persistNotify == nil {
		return
	}
	select {
	case session.persistNotify <- struct{}{}:
	default:
	}
}

func (m *Manager) stopOutputPersistence(session *managedSession) error {
	return m.stopOutputPersistenceContext(context.Background(), session)
}

func (m *Manager) stopOutputPersistenceContext(ctx context.Context, session *managedSession) error {
	if ctx == nil {
		return ErrInvalidSession
	}
	session.persistMu.Lock()
	defer session.persistMu.Unlock()
	if session.persistDone == nil {
		return nil
	}
	if session.persistStop != nil && !session.persistStopClosed {
		close(session.persistStop)
		session.persistStopClosed = true
	}
	select {
	case err := <-session.persistDone:
		session.persistNotify, session.persistStop, session.persistDone = nil, nil, nil
		session.persistStopClosed = false
		return classifySessionFailure("lifecycle", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) runOutputPersistence(session *managedSession, notify, stop <-chan struct{}, done chan<- error) {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	dirty := false
	flush := func() error {
		for {
			earliest, latest, _ := session.history.Bounds()
			_, persistedLatest, err := m.config.Store.OutputBounds(context.Background(), session.id)
			if err != nil {
				return err
			}
			if m.config.Metrics != nil {
				lag := uint64(0)
				if persistedLatest < latest {
					lag = latest - persistedLatest
				}
				_ = m.config.Metrics.Record("paperboat_runtime_terminal_persistence_lag_bytes", float64(lag), nil)
			}
			if persistedLatest < earliest {
				if err := m.config.Store.AdvanceOutput(context.Background(), session.id, persistedLatest, earliest); err != nil {
					return err
				}
				persistedLatest = earliest
			}
			if persistedLatest >= latest {
				return nil
			}
			replay, err := session.history.ReplayOwned(persistedLatest, m.config.HistoryBytes)
			if err != nil {
				var gap *history.GapError
				if errors.As(err, &gap) {
					continue
				}
				return err
			}
			for _, event := range replay.Events {
				if _, _, err := m.config.Store.AppendOutput(context.Background(), session.id, event.Channel, event.StartSequence, event.Data, m.config.HistoryBytes); err != nil {
					replay.Release()
					return err
				}
			}
			replay.Release()
			if replay.ToSequence >= latest {
				return nil
			}
		}
	}
	for {
		select {
		case <-notify:
			if !dirty {
				dirty = true
				timer.Reset(outputPersistDebounce)
			}
		case <-timer.C:
			if dirty {
				if err := flush(); err != nil {
					done <- err
					return
				}
				dirty = false
			}
		case <-stop:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			done <- flush()
			return
		}
	}
}
