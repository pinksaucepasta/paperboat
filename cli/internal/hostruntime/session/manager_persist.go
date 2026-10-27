package session

import (
	"context"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

const (
	// outputCommitInterval groups every session's pending output into one transaction, so the
	// host's fsync rate is about 1/interval however many terminals print at once.
	outputCommitInterval = 50 * time.Millisecond
	// outputCommitBytes bounds one group commit; sessions past it wait for the next one.
	outputCommitBytes = 256 << 10
	// outputFlushWait bounds how long a close or shutdown waits for its output to be persisted.
	// Output the disk could not take in time becomes a replay gap; a close never hangs on disk.
	outputFlushWait = 2 * time.Second
)

// outputWriter is the manager's one writer of terminal output to the runtime store. Output waits
// in each session's bounded history ring; when the disk falls behind, the oldest unpersisted
// output becomes a replay gap instead of growing memory.
type outputWriter struct {
	manager   *Manager
	mu        sync.Mutex
	dirty     map[*managedSession]struct{}
	decisions []store.InputDecision // input outcomes, written with the next group commit
	wake      chan struct{}
	committed chan struct{}
}

func (m *Manager) writer() *outputWriter {
	m.writerOnce.Do(func() {
		m.output = &outputWriter{manager: m, dirty: map[*managedSession]struct{}{}, wake: make(chan struct{}, 1), committed: make(chan struct{})}
		go m.output.run()
	})
	return m.output
}

func (m *Manager) startOutputPersistence(session *managedSession) {
	if m.config.Store == nil {
		return
	}
	_, latest, err := m.config.Store.OutputBounds(context.Background(), session.id)
	session.persistMu.Lock()
	session.persisting, session.persistedLatest, session.persistErr = err == nil, latest, err
	session.persistMu.Unlock()
}

func (m *Manager) queueOutputPersistence(session *managedSession, _ history.Event) {
	if m.config.Store == nil {
		return
	}
	m.writer().mark(session)
}

func (m *Manager) stopOutputPersistence(session *managedSession) error {
	return m.stopOutputPersistenceContext(context.Background(), session)
}

// stopOutputPersistenceContext persists the session's remaining output, waiting at most
// outputFlushWait (or ctx), then stops persisting it. Output that missed the wait is a replay gap.
func (m *Manager) stopOutputPersistenceContext(ctx context.Context, session *managedSession) error {
	if ctx == nil {
		return ErrInvalidSession
	}
	if m.config.Store == nil {
		return nil
	}
	session.persistMu.Lock()
	persisting, failed := session.persisting, session.persistErr
	session.persistMu.Unlock()
	if !persisting {
		// Stopped already, or stopped by a failed write whose cause the caller must see.
		return classifySessionFailure("lifecycle", failed)
	}
	wait, cancel := context.WithTimeout(ctx, outputFlushWait)
	defer cancel()
	err := m.writer().flush(wait, session)
	session.persistMu.Lock()
	session.persisting = false
	persistErr := session.persistErr
	session.persistMu.Unlock()
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && m.config.Metrics != nil {
		_ = m.config.Metrics.Record("paperboat_runtime_terminal_persistence_gaps_total", 1, nil)
	}
	return classifySessionFailure("lifecycle", persistErr)
}

// decided queues an input outcome for the next group commit. The input was recorded as uncertain before the PTY
// write, so a crash before this commit can never replay it; it only reads as uncertain after a restart.
func (w *outputWriter) decided(decision store.InputDecision) {
	w.mu.Lock()
	w.decisions = append(w.decisions, decision)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *outputWriter) mark(session *managedSession) {
	w.mu.Lock()
	w.dirty[session] = struct{}{}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// flush waits until the session's output up to now is persisted or ctx ends.
func (w *outputWriter) flush(ctx context.Context, session *managedSession) error {
	_, target, _ := session.history.Bounds()
	for {
		session.persistMu.Lock()
		done, err := session.persistedLatest >= target || !session.persisting, session.persistErr
		session.persistMu.Unlock()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		w.mu.Lock()
		committed := w.committed
		w.mu.Unlock()
		w.mark(session)
		select {
		case <-committed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (w *outputWriter) run() {
	for range w.wake {
		time.Sleep(outputCommitInterval)
		w.commit()
	}
}

type pendingOutput struct {
	session *managedSession
	batch   store.OutputBatch
	replay  history.Replay
	latest  uint64
}

// commit writes one group of pending output and wakes flush waiters.
func (w *outputWriter) commit() {
	w.mu.Lock()
	sessions := make([]*managedSession, 0, len(w.dirty))
	for session := range w.dirty {
		sessions = append(sessions, session)
	}
	w.dirty = map[*managedSession]struct{}{}
	decisions := w.decisions
	w.decisions = nil
	w.mu.Unlock()
	if len(decisions) > 0 {
		if err := w.manager.config.Store.UpdateInputDecisions(context.Background(), decisions); err != nil && w.manager.config.Metrics != nil {
			_ = w.manager.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
		}
	}
	var pending []pendingOutput
	var batches []store.OutputBatch
	budget := outputCommitBytes
	for _, session := range sessions {
		if budget <= 0 {
			w.mark(session)
			continue
		}
		item, ok := w.collect(session, budget)
		if !ok {
			continue
		}
		budget -= int(item.latest - item.batch.AdvanceTo)
		pending = append(pending, item)
		batches = append(batches, item.batch)
	}
	results := w.write(pending, batches)
	failed := false
	for i, item := range pending {
		item.replay.Release()
		item.session.persistMu.Lock()
		if results[i] == nil {
			item.session.persistedLatest = item.latest
		} else {
			// A session whose output cannot be written stops persisting with the cause, as the
			// per-session persister did; close and shutdown report it and may retry.
			item.session.persistErr, item.session.persisting = results[i], false
			failed = true
		}
		persisting := item.session.persisting
		item.session.persistMu.Unlock()
		if _, latest, _ := item.session.history.Bounds(); persisting && latest > item.latest {
			w.mark(item.session)
		}
	}
	if failed && w.manager.config.Metrics != nil {
		_ = w.manager.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
	}
	w.mu.Lock()
	close(w.committed)
	w.committed = make(chan struct{})
	w.mu.Unlock()
}

// write commits every batch in one transaction. If that fails, each session's batch is retried on
// its own so one session's failure does not stop the others.
func (w *outputWriter) write(pending []pendingOutput, batches []store.OutputBatch) []error {
	results := make([]error, len(pending))
	if len(batches) == 0 {
		return results
	}
	err := w.manager.config.Store.AppendOutputBatch(context.Background(), batches, w.manager.config.HistoryBytes)
	if err == nil {
		return results
	}
	if len(batches) == 1 {
		results[0] = err
		return results
	}
	for i, batch := range batches {
		results[i] = w.manager.config.Store.AppendOutputBatch(context.Background(), []store.OutputBatch{batch}, w.manager.config.HistoryBytes)
	}
	return results
}

// collect reads what the session has not persisted yet, up to budget bytes. Output older than
// the session's history ring is gone: the batch advances past it and replay reports the gap.
func (w *outputWriter) collect(session *managedSession, budget int) (pendingOutput, bool) {
	session.persistMu.Lock()
	persisting, cursor := session.persisting, session.persistedLatest
	session.persistMu.Unlock()
	if !persisting {
		return pendingOutput{}, false
	}
	earliest, latest, _ := session.history.Bounds()
	if cursor >= latest {
		return pendingOutput{}, false
	}
	item := pendingOutput{session: session, batch: store.OutputBatch{SessionID: session.id, AdvanceTo: cursor}}
	if cursor < earliest {
		item.batch.Advance, item.batch.AdvanceFrom, item.batch.AdvanceTo = true, cursor, earliest
		cursor = earliest
		if w.manager.config.Metrics != nil {
			_ = w.manager.config.Metrics.Record("paperboat_runtime_terminal_persistence_gaps_total", 1, nil)
		}
	}
	replay, err := session.history.ReplayOwned(cursor, uint64(min(budget, int(w.manager.config.HistoryBytes))))
	if err != nil {
		w.mark(session)
		return pendingOutput{}, false
	}
	item.replay, item.latest = replay, cursor
	for _, event := range replay.Events {
		item.batch.Events = append(item.batch.Events, store.OutputEvent{Channel: event.Channel, StartSequence: event.StartSequence, EndSequence: event.EndSequence, Data: event.Data})
		item.latest = event.EndSequence
	}
	if item.latest == cursor && !item.batch.Advance {
		replay.Release()
		return pendingOutput{}, false
	}
	return item, true
}
