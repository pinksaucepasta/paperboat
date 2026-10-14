package session

import (
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

// StoppedSnapshot matches a recovered stopped session without opening its PTY,
// replay, input journal, or command environment.
func StoppedSnapshot(record store.Session) (Snapshot, error) {
	state := State(record.State)
	if state == Creating || state == Running || state == Restarting || state == Closing {
		return Snapshot{}, ErrUpdateBusy
	}
	if state != Closed && state != Exited {
		return Snapshot{}, ErrInvalidSession
	}
	if record.ID == "" || record.Name == "" || record.Generation == 0 || record.EarliestSequence > record.LatestSequence {
		return Snapshot{}, ErrInvalidSession
	}
	snapshot := Snapshot{ID: record.ID, Name: record.Name, CWD: record.CWD,
		Dimensions: pty.Dimensions{Columns: record.Columns, Rows: record.Rows},
		State:      state, Generation: record.Generation,
		EarliestSequence: record.EarliestSequence, LatestSequence: record.LatestSequence}
	if record.ExitCode != nil {
		snapshot.Exit = &pty.ExitResult{Code: *record.ExitCode, Signal: record.ExitSignal}
		if record.ExitedAt != nil {
			snapshot.Exit.ExitedAt = *record.ExitedAt
		}
	}
	return snapshot, nil
}
