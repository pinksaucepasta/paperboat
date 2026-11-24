package session

import (
	"context"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/pty"
)

// A crash during recovery repeats it, so its commits need no sync each: 3 per session on one connection.
func (m *Manager) recover(ctx context.Context) error {
	return m.config.Store.Unsynced(ctx, func() error { return m.recoverSessions(ctx) })
}

func (m *Manager) recoverSessions(ctx context.Context) error {
	records, err := m.config.Store.Sessions(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		original := State(record.State)
		recovered := original
		now := time.Now().UTC()
		switch original {
		case Creating:
			recovered = Closed
		case Running, Restarting:
			recovered = Exited
			code := 255
			record.ExitCode, record.ExitSignal, record.ExitedAt = &code, m.config.RecoveryExitSignal, &now
		case Closing:
			recovered = Closed
		case Exited, Closed:
		default:
			return ErrInvalidSession
		}
		if recovered != original {
			record.State = string(recovered)
			if err := m.config.Store.UpdateSession(ctx, record.ID, string(original), record); err != nil {
				return err
			}
		}
		// Only an already-stopped record that remains stopped proves a clean
		// archive. In-flight records recovered as exited or closed were
		// interrupted and must keep the ordinary crash-recovery cleanup.
		confirmedStopped := recovered == original && (original == Exited || original == Closed)
		if (recovered == Exited || recovered == Closed) && !(m.config.PreserveStoppedHistory && confirmedStopped) {
			if err := m.config.Store.ClearStoppedOutput(ctx, record.ID, record.LatestSequence); err != nil {
				return err
			}
			record.EarliestSequence = record.LatestSequence
		}
		earliestSequence, err := m.config.Store.TrimOutput(ctx, record.ID, m.config.HistoryBytes)
		if err != nil {
			return err
		}
		storedEvents, earliest, latest, err := m.config.Store.Replay(ctx, record.ID, earliestSequence, 0)
		if err != nil {
			return err
		}
		events := make([]history.Event, len(storedEvents))
		for i, event := range storedEvents {
			events[i] = history.Event{Channel: event.Channel, StartSequence: event.StartSequence, EndSequence: event.EndSequence, Data: event.Data}
		}
		retained, err := history.Restore(m.config.HistoryBytes, earliest, latest, events)
		if err != nil {
			return err
		}
		lifecycle, err := RecoverLifecycle(recovered, record.Generation)
		if err != nil {
			return err
		}
		inputJournal := NewBoundedInputJournal(record.Generation, m.config.MaxInputDecisions)
		session := &managedSession{id: record.ID, name: record.Name, command: pty.Command{Path: record.CommandPath, Args: record.CommandArgs, Env: record.CommandEnv, CWD: record.CWD, Dimensions: pty.Dimensions{Columns: record.Columns, Rows: record.Rows}}, lifecycle: lifecycle, persistedState: recovered, history: retained, fanout: NewFanout(), inputs: inputJournal, participants: make(map[string]Participant), sizes: newWindowSizes()}
		if record.ExitCode != nil {
			session.exit = &pty.ExitResult{Code: *record.ExitCode, Signal: record.ExitSignal}
			if record.ExitedAt != nil {
				session.exit.ExitedAt = *record.ExitedAt
			}
		}
		if _, duplicate := m.sessions[record.ID]; duplicate || m.names[record.Name] != "" {
			return ErrInvalidSession
		}
		m.sessions[record.ID], m.names[record.Name] = session, record.ID
	}
	return nil
}

type contextInputWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextInputWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if writer, ok := w.writer.(interface {
		WriteContext(context.Context, []byte) (int, error)
	}); ok {
		return writer.WriteContext(w.ctx, data)
	}
	return w.writer.Write(data)
}
