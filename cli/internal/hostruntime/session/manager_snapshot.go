package session

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

func (m *Manager) Snapshot(sessionID string) (Snapshot, error) {
	return m.SnapshotAtGeneration(sessionID, 0)
}

func (m *Manager) SnapshotAtGeneration(sessionID string, expectedGeneration uint64) (Snapshot, error) {
	session, err := m.get(sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	session.opMu.Lock()
	snapshot := session.snapshotLocked()
	process := session.process
	session.opMu.Unlock()
	if expectedGeneration != 0 && snapshot.Generation != expectedGeneration {
		return Snapshot{}, &StaleGenerationError{CurrentGeneration: snapshot.Generation}
	}
	// Native observation is request-time only, outside the output/input lock.
	if observer, ok := process.(interface{ Identification() pty.Identification }); ok && (snapshot.State == Running || snapshot.State == Closing) {
		observed := observer.Identification()
		session.opMu.Lock()
		state, generation := session.lifecycle.Snapshot()
		current := session.process == process && generation == snapshot.Generation && (state == Running || state == Closing)
		session.opMu.Unlock()
		if current {
			snapshot.ForegroundProcess = cleanIdentification(observed.ForegroundProcess, 128)
			if observed.CurrentDirectory != "" {
				snapshot.CurrentDirectory = cleanIdentification(observed.CurrentDirectory, 1024)
			}
		} else {
			return m.SnapshotAtGeneration(sessionID, expectedGeneration)
		}
	}
	return snapshot, nil
}

func (m *Manager) List() []Snapshot {
	m.mu.RLock()
	sessions := make([]*managedSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()
	snapshots := make([]Snapshot, 0, len(sessions))
	for _, session := range sessions {
		if snapshot, err := m.Snapshot(session.id); err == nil {
			snapshots = append(snapshots, snapshot)
		}
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Name < snapshots[j].Name })
	return snapshots
}

func (m *Manager) get(sessionID string) (*managedSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionUnknown
	}
	return session, nil
}

func (s *managedSession) snapshotLocked() Snapshot {
	state, generation := s.lifecycle.Snapshot()
	earliest, latest, _ := s.history.Bounds()
	snapshot := Snapshot{ID: s.id, Name: s.name, CWD: s.command.CWD, Dimensions: s.command.Dimensions, State: state, Generation: generation, EarliestSequence: earliest, LatestSequence: latest, TerminalModes: s.modes.Modes()}
	snapshot.Title = s.identification.title
	if state == Running || state == Closing {
		snapshot.CurrentDirectory = s.identification.directory

	}
	for _, participant := range s.participants {
		state, _, err := s.fanout.Status(participant.AttachmentID)
		if err == nil && state == Attached {
			snapshot.Participants = append(snapshot.Participants, participant)
		}
	}
	sort.Slice(snapshot.Participants, func(i, j int) bool {
		return snapshot.Participants[i].AttachmentID < snapshot.Participants[j].AttachmentID
	})
	if s.exit != nil {
		exit := *s.exit
		snapshot.Exit = &exit
	}
	return snapshot
}

func (s *managedSession) storeRecordLocked() store.Session {
	snapshot := s.snapshotLocked()
	record := store.Session{ID: snapshot.ID, Name: snapshot.Name, CWD: snapshot.CWD, CommandPath: s.command.Path, CommandArgs: append([]string(nil), s.command.Args...), CommandEnv: append([]string(nil), s.command.Env...), Columns: snapshot.Dimensions.Columns, Rows: snapshot.Dimensions.Rows, State: string(snapshot.State), Generation: snapshot.Generation, EarliestSequence: snapshot.EarliestSequence, LatestSequence: snapshot.LatestSequence}
	if snapshot.Exit != nil {
		code := snapshot.Exit.Code
		record.ExitCode, record.ExitSignal = &code, snapshot.Exit.Signal
		exitedAt := snapshot.Exit.ExitedAt
		record.ExitedAt = &exitedAt
	}
	return record
}

func (m *Manager) persist(ctx context.Context, session *managedSession) error {
	if m.config.Store == nil {
		return nil
	}
	err := m.config.Store.UpdateSession(ctx, session.id, string(session.persistedState), session.storeRecordLocked())
	if err == nil {
		session.persistedState, _ = session.lifecycle.Snapshot()
	}
	return classifySessionFailure("lifecycle", err)
}

func (m *Manager) discardFailedCreate(ctx context.Context, session *managedSession, expected State) error {
	if m.config.Store == nil {
		return nil
	}
	updateErr := m.config.Store.UpdateSession(ctx, session.id, string(expected), session.storeRecordLocked())
	deleteErr := m.config.Store.DeleteSession(ctx, session.id)
	return classifySessionFailure("lifecycle", errors.Join(updateErr, deleteErr))
}

func randomID(random io.Reader) (string, error) {
	id, err := uuid.NewRandomFromReader(random)
	if err != nil {
		return "", err
	}
	return "terminal_" + id.String(), nil
}

func validSessionName(name string) bool {
	if len(name) < 1 || len(name) > 64 {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return !strings.HasPrefix(name, "-")
}
