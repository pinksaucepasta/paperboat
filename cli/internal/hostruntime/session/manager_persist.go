package session

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
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
	// outputRetryMin is the wait after a first failed write; it doubles per failure up to 32 s.
	outputRetryMin = 250 * time.Millisecond
)

// outputWriter is the manager's one writer of terminal output to the runtime store. Output waits
// in each session's bounded history ring; when the disk falls behind, the oldest unpersisted
// output becomes a replay gap instead of growing memory.
type outputWriter struct {
	manager   *Manager
	mu        sync.Mutex
	dirty     map[*managedSession]struct{}
	urgent    map[*managedSession]int // sessions a close or shutdown is waiting on; written first
	wake      chan struct{}
	committed chan struct{}
	done      chan struct{} // closed when the manager stops; run returns and flush gives up
	stopOnce  sync.Once
}

func newOutputWriter(m *Manager) *outputWriter {
	return &outputWriter{manager: m, dirty: map[*managedSession]struct{}{}, urgent: map[*managedSession]int{}, wake: make(chan struct{}, 1), committed: make(chan struct{}), done: make(chan struct{})}
}

func (m *Manager) writer() *outputWriter {
	m.writerOnce.Do(func() {
		m.output = newOutputWriter(m)
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
	session.persisting, session.persistedLatest, session.persistErr = true, latest, nil
	session.persistCursorLost, session.persistFailures, session.persistRetryAt = false, 0, time.Time{}
	session.persistMu.Unlock()
	if err != nil {
		m.writer().failed(session, err)
	}
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
		// Stopped already, or stopped by a permanent failure whose cause the caller must see.
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
	// A close waits for its own backlog, not for every flooding neighbour's: see commit.
	w.mu.Lock()
	w.urgent[session]++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.urgent[session]--; w.urgent[session] == 0 {
			delete(w.urgent, session)
		}
		w.mu.Unlock()
	}()
	_, target, _ := session.history.Bounds()
	session.persistMu.Lock()
	failures := session.persistFailures // a close retries once past an earlier failure
	session.persistMu.Unlock()
	for {
		session.persistMu.Lock()
		persisting, err := session.persisting, session.persistErr
		done := session.persistedLatest >= target && !session.persistCursorLost
		failedNow := session.persistFailures > failures
		session.persistMu.Unlock()
		if !persisting || failedNow {
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
		case <-w.done:
			return ErrManagerStopped
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (w *outputWriter) stop() { w.stopOnce.Do(func() { close(w.done) }) }

func (w *outputWriter) run() {
	for {
		select {
		case <-w.wake:
		case <-w.done:
			return
		}
		//paperboat:allow-source-policy sleep owner=hostruntime-session reason=grouped-commit-batch-pacing
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
	defer diagnostics.UpdateStep("terminal_output_commit")()
	w.mu.Lock()
	sessions := make([]*managedSession, 0, len(w.dirty))
	for session := range w.dirty {
		sessions = append(sessions, session)
	}
	w.dirty = map[*managedSession]struct{}{}
	// A close waits for this commit: write only the sessions someone waits on when there are any,
	// so its latency is their backlog, not a full group commit of every flooding neighbour.
	var deferred []*managedSession
	if len(w.urgent) > 0 {
		kept := sessions[:0]
		for _, session := range sessions {
			if w.urgent[session] > 0 {
				kept = append(kept, session)
			} else {
				deferred = append(deferred, session)
			}
		}
		sessions = kept
	}
	w.mu.Unlock()
	for _, session := range deferred {
		w.mark(session)
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
	for i, item := range pending {
		item.replay.Release()
		if results[i] != nil {
			w.failed(item.session, results[i])
			continue
		}
		item.session.persistMu.Lock()
		item.session.persistedLatest = item.latest
		if item.session.persisting { // a close that stopped waiting keeps its cause
			item.session.persistErr, item.session.persistFailures = nil, 0
		}
		persisting := item.session.persisting
		item.session.persistMu.Unlock()
		if _, latest, _ := item.session.history.Bounds(); persisting && latest > item.latest {
			w.mark(item.session)
		}
	}
	w.mu.Lock()
	close(w.committed)
	w.committed = make(chan struct{})
	w.mu.Unlock()
}

// failed schedules the session's next write after a backoff and reports the first failure of a spell.
// A closed store or a deleted session can never take the output: that session stops persisting.
func (w *outputWriter) failed(session *managedSession, err error) {
	session.persistMu.Lock()
	session.persistErr, session.persistCursorLost = err, true
	session.persistFailures++
	first := session.persistFailures == 1
	wait := outputRetryMin << min(session.persistFailures-1, 7)
	session.persistRetryAt = time.Now().Add(wait)
	if errors.Is(err, sql.ErrNoRows) || w.manager.config.Store.Closed() {
		session.persisting = false
	}
	retry := session.persisting
	session.persistMu.Unlock()
	if retry {
		time.AfterFunc(wait, func() { w.mark(session) })
	}
	if !first {
		return
	}
	observeSessionFailure(session, "lifecycle", classifySessionFailure("lifecycle", err))
	if w.manager.config.Metrics != nil {
		_ = w.manager.config.Metrics.Record("paperboat_runtime_terminal_persistence_failures_total", 1, nil)
	}
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
	if len(batches) == 1 || store.Busy(err) { // a busy store would cost every session its own busy_timeout
		for i := range results {
			results[i] = err
		}
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
	persisting, cursor, lost := session.persisting, session.persistedLatest, session.persistCursorLost
	waiting := session.persistFailures > 0 && time.Now().Before(session.persistRetryAt)
	session.persistMu.Unlock()
	w.mu.Lock()
	waiting = waiting && w.urgent[session] == 0
	w.mu.Unlock()
	if !persisting || waiting { // the retry timer of failed marks a waiting session again
		return pendingOutput{}, false
	}
	if lost {
		_, stored, err := w.manager.config.Store.OutputBounds(context.Background(), session.id)
		if err != nil {
			w.failed(session, err)
			return pendingOutput{}, false
		}
		cursor = stored
		session.persistMu.Lock()
		session.persistedLatest, session.persistCursorLost = stored, false
		if _, latest, _ := session.history.Bounds(); stored >= latest { // nothing left to write: the failed commit had landed
			session.persistErr, session.persistFailures = nil, 0
		}
		session.persistMu.Unlock()
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
