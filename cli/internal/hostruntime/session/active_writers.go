package session

import (
	"context"
	"time"
)

// ActiveWriter is private owner metadata. Terminal snapshots never expose the
// source machine mapping; every carrier and file handler reads the same owner.
type ActiveWriter struct {
	ClientID  string
	MachineID string
	LastInput time.Time
}

func (m *Manager) ActiveWriters(sessionID string) ([]ActiveWriter, error) {
	s, err := m.get(sessionID)
	if err != nil {
		return nil, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	writers := make([]ActiveWriter, 0, len(s.participants))
	for id, participant := range s.participants {
		if participant.Role == "viewer" || !s.fanout.IsAttached(id) {
			continue
		}
		writers = append(writers, ActiveWriter{ClientID: participant.ClientID, MachineID: participant.SourceMachineID, LastInput: s.sizes.inputTime(id)})
	}
	return writers, nil
}

func (m *RemoteManager) ActiveWriters(sessionID string) ([]ActiveWriter, error) {
	var out struct{ V0 []ActiveWriter }
	err := m.invoke(context.Background(), "ActiveWriters", struct{ Sessionid string }{sessionID}, &out)
	return out.V0, err
}

func (w *windowSizes) noteInput(id string, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if window := w.windows[id]; window != nil && window.attached {
		window.lastInput = at
	}
}
func (w *windowSizes) inputTime(id string) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if window := w.windows[id]; window != nil {
		return window.lastInput
	}
	return time.Time{}
}
