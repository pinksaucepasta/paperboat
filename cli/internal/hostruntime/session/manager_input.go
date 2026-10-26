package session

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

func (m *Manager) Write(sessionID string, key InputKey, data []byte) (InputDecision, error) {
	return m.WriteContext(context.Background(), sessionID, key, data)
}

func (m *Manager) WriteContext(ctx context.Context, sessionID string, key InputKey, data []byte) (InputDecision, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return InputDecision{}, err
	}
	// Input orders only against other input. It never takes opMu, which output capture holds for
	// every PTY chunk: under a flood (yes) a keystroke or Ctrl-C would never get it. The live
	// process is published atomically and cleared before Closing, so a running generation is all
	// input needs; the journal and fanout have their own locks.
	// A lone interrupt key that finds input busy (a paste the program never reads) is delivered the way the line
	// discipline would, ahead of the stuck write; it keeps its journal entry but its byte is not written again.
	interrupted := false
	if !session.inputMu.TryLock() {
		if len(data) == 1 {
			interrupted = interruptStuckInput(session, data[0])
		}
		if err := pty.LockInput(ctx, &session.inputMu); err != nil {
			return InputDecision{}, err
		}
	}
	defer session.inputMu.Unlock()
	live := session.liveProcess.Load()
	if live == nil || key.Generation != live.generation {
		_, generation := session.lifecycle.Snapshot()
		return InputDecision{}, &StaleGenerationError{CurrentGeneration: generation}
	}
	var process io.Writer = live.process
	if interrupted {
		process = deliveredInput{}
	}
	if attachmentState, _, err := session.fanout.Status(key.AttachmentID); err != nil || attachmentState != Attached {
		return InputDecision{}, ErrInvalidInput
	}
	key, _ = normalizeInputKey(key)
	if _, queryErr := session.inputs.Query(key); queryErr == nil {
		return session.inputs.Write(key, data, contextInputWriter{ctx: ctx, writer: process})
	} else if !errors.Is(queryErr, ErrInputUnknown) {
		return InputDecision{}, queryErr
	}
	if err := session.inputs.Admit(key); err != nil {
		return InputDecision{}, err
	}
	hash := sha256.Sum256(data)
	storageID := inputStorageID(key)
	if m.config.Store != nil {
		inserted, err := m.config.Store.PutInputDecision(context.Background(), store.InputDecision{SessionID: sessionID, ClientID: key.ClientID, AttachmentID: key.AttachmentID, Generation: key.Generation, InputID: storageID, Hash: hash[:], Status: string(InputUncertain), CreatedAt: time.Now().UTC()})
		if err != nil {
			return InputDecision{}, err
		}
		if !inserted {
			return InputDecision{}, ErrInputConflict
		}
	}
	decision, err := session.inputs.Write(key, data, contextInputWriter{ctx: ctx, writer: process})
	if err != nil {
		return decision, err
	}
	if m.config.Store != nil {
		persisted := store.InputDecision{SessionID: sessionID, ClientID: key.ClientID, AttachmentID: key.AttachmentID, Generation: key.Generation, InputID: storageID, Hash: hash[:], Status: string(decision.Status), BytesWritten: decision.BytesWritten, ErrorCode: decision.WriteError}
		if persistErr := m.config.Store.UpdateInputDecision(context.Background(), persisted); persistErr != nil {
			decision.Status = InputUncertain
			decision.WriteError = "decision_persist_failed"
			return decision, persistErr
		}
		// The journal keeps only the last InputDecisionWindow sequenced decisions of a client; the
		// durable rows follow it in batches so a long session stays bounded (Lane X audit H2).
		if key.InputSequence > InputDecisionWindow && key.InputSequence%inputPruneBatch == 0 {
			if pruneErr := m.config.Store.PruneInputDecisions(context.Background(), sessionID, key.ClientID, key.AttachmentID, key.Generation, key.InputSequence-InputDecisionWindow); pruneErr != nil {
				observeSessionFailure(session, "lifecycle", classifySessionFailure("lifecycle", pruneErr))
			}
		}
	}
	return decision, nil
}

type interrupter interface{ Interrupt(byte) (bool, error) }

func interruptStuckInput(session *managedSession, key byte) bool {
	live := session.liveProcess.Load()
	if live == nil {
		return false
	}
	process, ok := live.process.(interrupter)
	if !ok {
		return false
	}
	handled, err := process.Interrupt(key)
	if err != nil {
		observeSessionFailure(session, "input", classifySessionFailure("input", err))
	}
	return handled
}

// deliveredInput stands in for the PTY once Interrupt has delivered the byte.
type deliveredInput struct{}

func (deliveredInput) Write(data []byte) (int, error) { return len(data), nil }

// inputPruneBatch is how many sequenced writes pass between deletions of durable decisions that left
// the window.
const inputPruneBatch = 64

// InputSequence returns the highest sequence recorded for an attachment. The
// stream server uses it when rebinding a persistent attachment after a
// transport reconnect so a duplicate delivery is answered from the journal.
func (m *Manager) InputSequence(sessionID, clientID, attachmentID string, generation uint64) (uint64, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return 0, err
	}
	return session.inputs.LastSequence(clientID, attachmentID, generation), nil
}

func inputStorageID(key InputKey) string {
	if key.InputSequence != 0 {
		return strconv.FormatUint(key.InputSequence, 10)
	}
	return key.InputID
}

// WriteStream writes ordered live terminal input without creating an
// idempotency decision. Stream input is never replayed after disconnection.
func (m *Manager) WriteStream(sessionID, attachmentID string, generation uint64, data []byte) error {
	return m.WriteStreamContext(context.Background(), sessionID, attachmentID, generation, data)
}

func (m *Manager) WriteStreamContext(ctx context.Context, sessionID, attachmentID string, generation uint64, data []byte) error {
	if len(data) == 0 || len(data) > 256<<10 {
		return ErrInvalidInput
	}
	session, err := m.get(sessionID)
	if err != nil {
		return err
	}
	if err := pty.LockInput(ctx, &session.inputMu); err != nil {
		return err
	}
	defer session.inputMu.Unlock()
	live := session.liveProcess.Load()
	if live == nil || generation != live.generation {
		_, currentGeneration := session.lifecycle.Snapshot()
		return &StaleGenerationError{CurrentGeneration: currentGeneration}
	}
	if !session.fanout.IsAttached(attachmentID) {
		return ErrInvalidInput
	}
	n, writeErr := (contextInputWriter{ctx: ctx, writer: live.process}).Write(data)
	if writeErr != nil {
		return writeErr
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (m *Manager) QueryInput(sessionID string, key InputKey) (InputDecision, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return InputDecision{}, err
	}
	return session.inputs.Query(key)
}

func (m *Manager) Resize(sessionID, attachmentID string, dimensions pty.Dimensions, activeAt time.Time) error {
	return m.resize(sessionID, attachmentID, "", "", 0, dimensions, activeAt)
}

func (m *Manager) ResizeParticipantAtGeneration(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64, dimensions pty.Dimensions, activeAt time.Time) error {
	return m.resize(sessionID, attachmentID, accountID, clientID, expectedGeneration, dimensions, activeAt)
}

func (m *Manager) resize(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64, dimensions pty.Dimensions, activeAt time.Time) error {
	session, err := m.get(sessionID)
	if err != nil {
		return err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if expectedGeneration != 0 {
		_, generation := session.lifecycle.Snapshot()
		p, ok := session.participants[attachmentID]
		if generation != expectedGeneration || !ok || p.AccountID != accountID || p.ClientID != clientID {
			return ErrStaleGeneration
		}
	}
	attachmentState, _, statusErr := session.fanout.Status(attachmentID)
	if statusErr != nil || attachmentState != Attached || session.process == nil {
		return ErrInvalidInput
	}
	if activeAt.Before(session.resizeTime) || activeAt.Equal(session.resizeTime) && attachmentID < session.resizeID {
		return nil
	}
	if session.command.Dimensions == dimensions {
		return nil
	}
	if err := session.process.Resize(dimensions); err != nil {
		return err
	}
	session.resizeID, session.resizeTime = attachmentID, activeAt
	session.command.Dimensions = dimensions
	if session.screen != nil {
		session.screen.Resize(int(dimensions.Columns), int(dimensions.Rows))
	}
	// Include geometry in the same ordered output history as PTY bytes. Every
	// attachment must interpret cursor-addressed output at the host's grid size.
	resizeOutput := []byte(fmt.Sprintf("\x1b[8;%d;%dt", dimensions.Rows, dimensions.Columns))
	event, err := session.history.Append(2, resizeOutput)
	if err != nil {
		return err
	}
	_, _ = session.fanout.PublishOwned(event)
	m.queueOutputPersistence(session, event)
	if err := m.persist(context.Background(), session); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Signal(sessionID string, generation uint64, signal pty.Signal) error {
	session, err := m.get(sessionID)
	if err != nil {
		return err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	state, current := session.lifecycle.Snapshot()
	if state != Running || generation != current || session.process == nil {
		return ErrStaleGeneration
	}
	return session.process.Signal(signal)
}

func (m *Manager) Clear(sessionID string) (uint64, error) {
	return m.clearAtGeneration(sessionID, 0)
}

func (m *Manager) ClearAtGeneration(sessionID string, expectedGeneration uint64) (uint64, error) {
	if expectedGeneration == 0 {
		return 0, ErrInvalidInput
	}
	return m.clearAtGeneration(sessionID, expectedGeneration)
}

func (m *Manager) clearAtGeneration(sessionID string, expectedGeneration uint64) (uint64, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return 0, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if err := m.checkRetiring(); err != nil {
		return 0, err
	}
	_, generation := session.lifecycle.Snapshot()
	if expectedGeneration != 0 && generation != expectedGeneration {
		return 0, &StaleGenerationError{CurrentGeneration: generation}
	}
	if err := m.clearOutputLocked(context.Background(), session, false); err != nil {
		return 0, err
	}
	_, latest, _ := session.history.Bounds()
	return latest, nil
}

// clearOutputLocked removes the replay tail without deleting the stable
// session identity. Callers hold opMu and have stopped output persistence for
// terminal states; an explicit live Clear may race only with a later append.
func (m *Manager) clearOutputLocked(ctx context.Context, session *managedSession, stopped bool) error {
	_, latest, _ := session.history.Bounds()
	var clearErr error
	if m.config.Store != nil {
		if stopped {
			clearErr = m.config.Store.ClearStoppedOutput(ctx, session.id, latest)
		} else {
			clearErr = m.config.Store.ClearOutput(ctx, session.id, latest)
		}
	}
	if clearErr == nil || stopped {
		// A stopped terminal must not offer its old output to a new attach,
		// even if durable cleanup failed. Recovery retries durable deletion.
		session.history.Clear()
		if stopped {
			session.fanout.Release()
		}
	}
	if stopped && session.screen != nil {
		session.screen.Dispose()
		session.screen = nil
		session.continuation.reset()
	}
	return classifySessionFailure("lifecycle", clearErr)
}
