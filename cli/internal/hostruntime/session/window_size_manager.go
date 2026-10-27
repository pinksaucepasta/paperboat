package session

import "time"

// registerWindowLocked enters a controller attachment (owner or interactive; a viewer never takes part) into the
// size rule and applies the size if it now controls. Attaching is not activity: the window that was last active
// keeps control. opMu is held.
func (m *Manager) registerWindowLocked(session *managedSession, participant Participant) {
	if participant.Role == "viewer" {
		return
	}
	session.sizes.attach(participant.AttachmentID, participant.Dimensions, time.Now())
	m.applyWindowSizeObservedLocked(session)
}

func (m *Manager) applyWindowSizeObservedLocked(session *managedSession) {
	if err := m.applyWindowSizeLocked(session); err != nil {
		observeSessionFailure(session, "lifecycle", classifySessionFailure("lifecycle", err))
	}
}

// noteWindowActivity is called for every input from an attachment. Typing makes the attachment's window the one
// that sets the PTY size; if that changes the controller the size is applied off the input path, which never waits
// for opMu (output capture holds it per chunk).
func (m *Manager) noteWindowActivity(session *managedSession, attachmentID string) {
	if !session.sizes.touch(attachmentID, time.Now()) {
		return
	}
	// Pacing moves with the typing at once; only the resize waits for opMu.
	session.fanout.SetController(attachmentID)
	session.sizeDirty.Store(true)
	if session.sizeApplying.CompareAndSwap(false, true) {
		go m.applyPendingWindowSize(session)
	}
}

func (m *Manager) applyPendingWindowSize(session *managedSession) {
	for {
		session.sizeDirty.Store(false)
		session.opMu.Lock()
		m.applyWindowSizeObservedLocked(session)
		session.opMu.Unlock()
		session.sizeApplying.Store(false)
		// A touch that landed after the dirty flag was cleared is applied by this loop, not lost.
		if !session.sizeDirty.Load() || !session.sizeApplying.CompareAndSwap(false, true) {
			return
		}
	}
}
