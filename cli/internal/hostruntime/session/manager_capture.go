package session

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
)

// ownerStallBound is how long the PTY waits for an owner that drains nothing before treating its link as gone.
const ownerStallBound = 5 * time.Second

func (m *Manager) capture(session *managedSession, process PTYProcess) {
	var readErr, deliveryErr error
	for {
		buffer := history.AcquireBuffer()
		n, err := process.Read(buffer)
		if n > 0 {
			// Outside opMu: input, resize and attach never wait for the owner to drain.
			session.fanout.WaitRoom(context.Background(), uint64(n), ownerStallBound)
			session.outputMu.Lock()
			session.opMu.Lock()
			state, _ := session.lifecycle.Snapshot()
			if session.process == process && (state == Running || state == Closing) {
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
			session.outputMu.Unlock()
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
	if deliveryErr == nil {
		result, waitErr = process.Wait(context.Background())
	}
	closeErr := process.CloseIO()
	observeSessionFailure(session, "delivery", classifySessionFailure("delivery", readErr))
	observeSessionFailure(session, "command", classifySessionFailure("command", waitErr))
	m.finishCapture(session, process, result, errors.Join(terminateErr, closeErr), deliveryErr, terminateErr)
}

func (m *Manager) finishCapture(
	session *managedSession, process PTYProcess, result pty.ExitResult,
	lifecycleErr, deliveryErr, terminateErr error,
) {
	session.operationMu.Lock()
	defer session.operationMu.Unlock()
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if session.process != process {
		observeSessionFailure(session, "lifecycle", lifecycleErr)
		return
	}
	session.opMu.Unlock()
	persistErr := m.stopOutputPersistence(session)
	session.opMu.Lock()
	lifecycleErr = errors.Join(lifecycleErr, persistErr)
	session.persistMu.Lock()
	session.persistErr = persistErr
	session.persistMu.Unlock()
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
				session.persistMu.Lock()
				session.persistErr = errors.Join(session.persistErr, err)
				session.persistMu.Unlock()
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
			session.persistMu.Lock()
			session.persistErr = errors.Join(session.persistErr, err)
			session.persistMu.Unlock()
			lifecycleErr = errors.Join(lifecycleErr, err)
			if m.config.Metrics != nil {
				_ = m.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
			}
		}
		if !m.config.PreserveStoppedHistory {
			if clearErr := m.clearOutputLocked(context.Background(), session, true); clearErr != nil {
				session.persistMu.Lock()
				session.persistErr = errors.Join(session.persistErr, clearErr)
				session.persistMu.Unlock()
				lifecycleErr = errors.Join(lifecycleErr, clearErr)
				if m.config.Metrics != nil {
					_ = m.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
				}
			}
		}
	}
	observeSessionFailure(session, "lifecycle", lifecycleErr)
}
