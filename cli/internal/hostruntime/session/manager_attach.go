package session

import (
	"context"

	xterm "github.com/gitpod-io/xterm-go"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func (m *Manager) Attach(sessionID, attachmentID string, fromSequence uint64) (AttachResult, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return AttachResult{}, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	return m.attachLocked(session, attachmentID, fromSequence, false, false)
}

func (m *Manager) AttachParticipant(sessionID string, participant Participant, fromSequence uint64) (AttachResult, error) {
	return m.AttachParticipantAtGeneration(sessionID, participant, fromSequence, 0)
}

func (m *Manager) AttachParticipantAtGeneration(sessionID string, participant Participant, fromSequence, expectedGeneration uint64) (AttachResult, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return AttachResult{}, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	if !validParticipant(participant, expectedGeneration) {
		return AttachResult{}, ErrInvalidInput
	}
	if participant.ConnectedAt.IsZero() {
		return AttachResult{}, ErrInvalidInput
	}
	_, generation := session.lifecycle.Snapshot()
	if expectedGeneration != 0 && expectedGeneration != generation || participant.Role != "owner" && expectedGeneration == 0 {
		return AttachResult{}, &StaleGenerationError{CurrentGeneration: generation}
	}
	m.pruneParticipantsLocked(session)
	if fromSequence == 0 {
		fromSequence, _, _ = session.history.Bounds()
	}
	result, err := m.attachLocked(session, participant.AttachmentID, fromSequence, participant.Browser, controllingOwner(participant))
	if err == nil {
		if session.participants == nil {
			session.participants = make(map[string]Participant)
		}
		session.participants[participant.AttachmentID] = participant
		m.registerWindowLocked(session, participant)
		result.Snapshot = session.snapshotLocked()
	}
	return result, err
}

func (m *Manager) pruneParticipantsLocked(session *managedSession) {
	for attachmentID := range session.participants {
		state, _, err := session.fanout.Status(attachmentID)
		if err != nil || state != Attached {
			if err == nil {
				_ = session.fanout.Detach(attachmentID)
			}
			session.removeParticipantLocked(attachmentID)
		}
	}
}

// AttachLive resolves the current output boundary and registers the attachment
// while holding the same session lock. High-output terminals therefore cannot
// compact past a boundary observed by a separate snapshot request.
func (m *Manager) AttachLive(sessionID, attachmentID string) (AttachResult, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return AttachResult{}, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	_, latest, _ := session.history.Bounds()
	return m.attachLocked(session, attachmentID, latest, false, false)
}

func (m *Manager) AttachLiveParticipant(sessionID string, participant Participant) (AttachResult, error) {
	return m.AttachLiveParticipantAtGeneration(sessionID, participant, 0)
}

func (m *Manager) AttachLiveParticipantAtGeneration(sessionID string, participant Participant, expectedGeneration uint64) (AttachResult, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return AttachResult{}, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	_, latest, _ := session.history.Bounds()
	if !validParticipant(participant, expectedGeneration) || participant.ConnectedAt.IsZero() {
		return AttachResult{}, ErrInvalidInput
	}
	_, generation := session.lifecycle.Snapshot()
	if expectedGeneration != 0 && expectedGeneration != generation || participant.Role != "owner" && expectedGeneration == 0 {
		return AttachResult{}, &StaleGenerationError{CurrentGeneration: generation}
	}
	m.pruneParticipantsLocked(session)
	result, err := m.attachLocked(session, participant.AttachmentID, latest, participant.Browser, controllingOwner(participant))
	if err == nil {
		if session.participants == nil {
			session.participants = make(map[string]Participant)
		}
		session.participants[participant.AttachmentID] = participant
		m.registerWindowLocked(session, participant)
		result.Snapshot = session.snapshotLocked()
	}
	return result, err
}

// BrowserScreenAttachment captures the host's current VT screen and moves one
// browser attachment to that exact output boundary. The checkpoint is kept in
// memory only and is sent on the authenticated, encrypted terminal stream.
func (m *Manager) BrowserScreenAttachment(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64) ([]byte, uint64, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return nil, 0, err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	_, generation := session.lifecycle.Snapshot()
	participant, ok := session.participants[attachmentID]
	if expectedGeneration == 0 || generation != expectedGeneration || !ok || participant.AccountID != accountID || participant.ClientID != clientID || session.screen == nil {
		return nil, 0, ErrInvalidInput
	}
	state, _, err := session.fanout.Status(attachmentID)
	if err != nil || state != Attached {
		return nil, 0, ErrInvalidInput
	}
	checkpoint := xterm.NewSerializeAddon(session.screen).Serialize(nil)
	if session.continuation.overflow {
		return nil, 0, ErrResourceLimit
	}
	checkpoint = append(checkpoint, session.continuation.pending...)
	if len(checkpoint) > protocol.MaxTerminalScreenCheckpointBytes {
		return nil, 0, ErrResourceLimit
	}
	_, latest, _ := session.history.Bounds()
	if err := session.fanout.SkipTo(attachmentID, latest); err != nil {
		return nil, 0, err
	}
	if err := session.history.Acknowledge(attachmentID, latest); err != nil {
		return nil, 0, err
	}
	return checkpoint, latest, nil
}

func validParticipant(participant Participant, expectedGeneration uint64) bool {
	validRole := participant.Role == "owner" || participant.Role == "viewer" || participant.Role == "interactive"
	if participant.AttachmentID == "" || !validRole {
		return false
	}
	if participant.Role != "owner" || expectedGeneration != 0 {
		return participant.AccountID != "" && participant.ClientID != ""
	}
	return true
}

// controllingOwner reports whether a participant's window may pace the PTY when the size rule makes it the
// controller: a native owner or interactive window. Viewers never control; a browser is fed by the shared publisher.
func controllingOwner(participant Participant) bool {
	return participant.Role != "viewer" && !participant.Browser
}

func (m *Manager) attachLocked(session *managedSession, attachmentID string, fromSequence uint64, controlOnly, backpressure bool) (AttachResult, error) {
	if err := m.checkRetiring(); err != nil {
		return AttachResult{}, err
	}
	firstAttachment := session.fanout.Count() == 0
	if session.fanout.Count() >= m.config.MaxAttachments {
		return AttachResult{}, ErrResourceLimit
	}
	if controlOnly {
		earliest, latest, _ := session.history.Bounds()
		if err := session.fanout.AttachControl(attachmentID, m.config.AttachmentBytes); err != nil {
			return AttachResult{}, err
		}
		if err := session.history.Acknowledge(attachmentID, latest); err != nil {
			_ = session.fanout.Detach(attachmentID)
			return AttachResult{}, err
		}
		return AttachResult{Snapshot: session.snapshotLocked(), Replay: history.Replay{FromSequence: latest, ToSequence: latest, EarliestSequence: earliest, LatestSequence: latest}, FirstAttachment: firstAttachment}, nil
	}
	replayLimit := attachmentReplayLimit(m.config.AttachmentBytes)
	replay, err := session.history.ReplayOwned(fromSequence, replayLimit)
	if err != nil {
		return AttachResult{}, err
	}
	// A durable session may retain more history than a live attachment can
	// queue. Start at the newest bounded tail so reconnect always succeeds and
	// presents the most recent output instead of repeatedly racing replay gaps.
	if replay.ToSequence < replay.LatestSequence {
		boundary := replay.LatestSequence - replayLimit
		if boundary < replay.EarliestSequence {
			boundary = replay.EarliestSequence
		}
		fromSequence = boundary
		replay.Release()
		replay, err = session.history.ReplayOwned(fromSequence, replayLimit)
		if err != nil {
			return AttachResult{}, err
		}
	}
	attach := session.fanout.Attach
	if backpressure {
		attach = session.fanout.AttachBackpressured
	}
	if err := attach(attachmentID, m.config.AttachmentBytes); err != nil {
		replay.Release()
		return AttachResult{}, err
	}
	for _, event := range replay.Events {
		if eviction, err := session.fanout.EnqueueOwned(attachmentID, event); err != nil {
			replay.Release()
			_ = session.fanout.Detach(attachmentID)
			return AttachResult{}, err
		} else if eviction != nil {
			replay.Release()
			_ = session.fanout.Detach(attachmentID)
			return AttachResult{}, ErrAttachmentEvicted
		}
	}
	publicReplay := cloneReplay(replay)
	replay.Release()
	return AttachResult{Snapshot: session.snapshotLocked(), Replay: publicReplay, FirstAttachment: firstAttachment}, nil
}

func cloneReplay(replay history.Replay) history.Replay {
	result := replay
	result.Events = make([]history.Event, len(replay.Events))
	for index, event := range replay.Events {
		result.Events[index] = history.Event{Channel: event.Channel, StartSequence: event.StartSequence, EndSequence: event.EndSequence, Data: append([]byte(nil), event.Data...)}
	}
	return result
}

func attachmentReplayLimit(attachmentBytes uint64) uint64 {
	limit := attachmentBytes / 2
	if limit == 0 {
		limit = 1
	}
	if limit > initialReplayBytes {
		return initialReplayBytes
	}
	return limit
}

func (m *Manager) Detach(sessionID, attachmentID string) error {
	session, err := m.get(sessionID)
	if err != nil {
		return err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	err = session.fanout.Detach(attachmentID)
	if err == nil {
		session.removeParticipantLocked(attachmentID)
		m.applyWindowSizeObservedLocked(session)
	}
	return err
}

func (m *Manager) DetachParticipantAtGeneration(sessionID, attachmentID, accountID, clientID string, expectedGeneration uint64) error {
	session, err := m.get(sessionID)
	if err != nil {
		return err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	_, generation := session.lifecycle.Snapshot()
	p, ok := session.participants[attachmentID]
	if expectedGeneration == 0 || generation != expectedGeneration || !ok || p.AccountID != accountID || p.ClientID != clientID {
		return ErrStaleGeneration
	}
	if err = session.fanout.Detach(attachmentID); err == nil {
		session.removeParticipantLocked(attachmentID)
		m.applyWindowSizeObservedLocked(session)
	}
	return err
}

func (m *Manager) AttachmentOwnedBy(sessionID, attachmentID, accountID, clientID string) bool {
	session, err := m.get(sessionID)
	if err != nil {
		return false
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	p, ok := session.participants[attachmentID]
	return ok && p.AccountID == accountID && p.ClientID == clientID
}

func (m *Manager) Next(sessionID, attachmentID string) (history.Event, bool, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return history.Event{}, false, err
	}
	return session.fanout.Next(attachmentID)
}

// WaitNext returns the attachment's next output at or after from, the end of what the caller already holds; see
// Fanout.WaitNextFrom. Nothing is consumed until a later call moves from past it.
func (m *Manager) WaitNext(ctx context.Context, sessionID, attachmentID string, from uint64) (history.Event, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return history.Event{}, err
	}
	return session.fanout.WaitNextFrom(ctx, attachmentID, from)
}

func (m *Manager) Acknowledge(sessionID, attachmentID string, nextSequence uint64) error {
	return m.acknowledgeAtGeneration(sessionID, attachmentID, nextSequence, 0)
}

func (m *Manager) AcknowledgeAtGeneration(sessionID, attachmentID string, nextSequence, expectedGeneration uint64) error {
	if expectedGeneration == 0 {
		return ErrInvalidInput
	}
	return m.acknowledgeAtGeneration(sessionID, attachmentID, nextSequence, expectedGeneration)
}

func (m *Manager) acknowledgeAtGeneration(sessionID, attachmentID string, nextSequence, expectedGeneration uint64) error {
	session, err := m.get(sessionID)
	if err != nil {
		return err
	}
	session.opMu.Lock()
	defer session.opMu.Unlock()
	_, generation := session.lifecycle.Snapshot()
	if expectedGeneration != 0 && generation != expectedGeneration {
		return &StaleGenerationError{CurrentGeneration: generation}
	}
	state, _, err := session.fanout.Status(attachmentID)
	if err != nil || state != Attached {
		return ErrInvalidInput
	}
	if acked, ok := session.history.Acknowledged(attachmentID); ok && nextSequence < acked {
		return nil // sent before a screen moved this window past it (F3-13); stale, not wrong
	}
	return session.history.Acknowledge(attachmentID, nextSequence)
}

func (m *Manager) AttachmentStatus(sessionID, attachmentID string) (AttachmentState, uint64, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return "", 0, err
	}
	return session.fanout.Status(attachmentID)
}
