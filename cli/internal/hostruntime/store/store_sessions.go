package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store/storesqlc"
)

func (s *Store) CreateSession(ctx context.Context, session Session) (resultErr error) {
	defer protectStoreError(&resultErr)
	now := time.Now().UTC()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = now
	}
	args, err := json.Marshal(session.CommandArgs)
	if err != nil {
		return err
	}
	environment, err := json.Marshal(session.CommandEnv)
	if err != nil {
		return err
	}
	err = s.q.CreateSession(ctx, storesqlc.CreateSessionParams{
		ID: session.ID, Name: session.Name, Cwd: session.CWD, CommandPath: session.CommandPath,
		CommandArgs: args, CommandEnv: environment, Columns: int64(session.Columns), Rows: int64(session.Rows),
		State: session.State, Generation: int64(session.Generation), ExitCode: nullableInt(session.ExitCode),
		ExitSignal: nullableSQLString(session.ExitSignal), ExitedAt: nullableTimestamp(session.ExitedAt),
		EarliestSequence: int64(session.EarliestSequence), LatestSequence: int64(session.LatestSequence),
		CreatedAt: session.CreatedAt.UnixNano(), UpdatedAt: session.UpdatedAt.UnixNano(),
	})
	return classify(err)
}

func (s *Store) Sessions(ctx context.Context) (sessionResults []Session, resultErr error) {
	defer protectStoreError(&resultErr)
	rows, err := s.q.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(rows))
	for _, row := range rows {
		session, err := sessionFromSQLC(row)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func (s *Store) UpdateSession(ctx context.Context, id, expectedState string, next Session) (resultErr error) {
	defer protectStoreError(&resultErr)
	// Output bounds are owned by AppendOutput and ClearOutput. Updating them
	// here can expose a cursor whose output rows have not committed yet.
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET cwd=?,columns=?,rows=?,state=?,generation=?,exit_code=?,exit_signal=?,exited_at=?,updated_at=? WHERE id=? AND state=?`, next.CWD, next.Columns, next.Rows, next.State, next.Generation, next.ExitCode, nullableString(next.ExitSignal), nullableTime(next.ExitedAt), time.Now().UTC().UnixNano(), id, expectedState)
	if err != nil {
		return err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return affectedErr
	}
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) ClearOutput(ctx context.Context, sessionID string, nextSequence uint64) (resultErr error) {
	defer protectStoreError(&resultErr)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer retainRollback(tx, &resultErr)
	var latest uint64
	if err := tx.QueryRowContext(ctx, `SELECT latest_sequence FROM sessions WHERE id=?`, sessionID).Scan(&latest); err != nil {
		return err
	}
	if latest != nextSequence {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM output_events WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET earliest_sequence=?,latest_sequence=?,updated_at=? WHERE id=?`, nextSequence, nextSequence, time.Now().UTC().UnixNano(), sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearStoppedOutput removes output after the session's writer has stopped.
// Its cursor may be ahead of the durable cursor when an asynchronous append
// failed, so preserving the larger value avoids replaying sequence numbers.
func (s *Store) ClearStoppedOutput(ctx context.Context, sessionID string, nextSequence uint64) (resultErr error) {
	defer protectStoreError(&resultErr)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer retainRollback(tx, &resultErr)
	var latest uint64
	if err := tx.QueryRowContext(ctx, `SELECT latest_sequence FROM sessions WHERE id=?`, sessionID).Scan(&latest); err != nil {
		return err
	}
	if latest > nextSequence {
		nextSequence = latest
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM output_events WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET earliest_sequence=?,latest_sequence=?,updated_at=? WHERE id=?`, nextSequence, nextSequence, time.Now().UTC().UnixNano(), sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) OutputBounds(ctx context.Context, sessionID string) (earliest, latest uint64, err error) {
	defer protectStoreError(&err)
	row, err := s.q.SessionBounds(ctx, sessionID)
	return uint64(row.EarliestSequence), uint64(row.LatestSequence), err
}

// AdvanceOutput drops a persistence tail that has already fallen behind the
// bounded live history. The expected cursor prevents concurrent writers from
// skipping an unseen committed range.
func (s *Store) AdvanceOutput(ctx context.Context, sessionID string, expectedLatest, nextSequence uint64) (resultErr error) {
	return s.AppendOutputBatch(ctx, []OutputBatch{{SessionID: sessionID, Advance: true, AdvanceFrom: expectedLatest, AdvanceTo: nextSequence}}, 1)
}

func advanceOutputTx(ctx context.Context, tx *sql.Tx, sessionID string, expectedLatest, nextSequence uint64) error {
	if nextSequence < expectedLatest {
		return ErrConflict
	}
	var latest uint64
	if err := tx.QueryRowContext(ctx, `SELECT latest_sequence FROM sessions WHERE id=?`, sessionID).Scan(&latest); err != nil {
		return err
	}
	if latest != expectedLatest {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM output_events WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET earliest_sequence=?,latest_sequence=?,updated_at=? WHERE id=?`, nextSequence, nextSequence, time.Now().UTC().UnixNano(), sessionID)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, id string) (resultErr error) {
	defer protectStoreError(&resultErr)
	changed, err := s.q.DeleteClosedSession(ctx, id)
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) AppendOutput(ctx context.Context, sessionID string, channel byte, start uint64, data []byte, maxRetained uint64) (outputResult OutputEvent, earliestResult uint64, resultErr error) {
	if len(data) == 0 || maxRetained == 0 {
		return OutputEvent{}, 0, ErrConflict
	}
	event := OutputEvent{Channel: channel, StartSequence: start, EndSequence: start + uint64(len(data)), Data: append([]byte(nil), data...)}
	if err := s.AppendOutputBatch(ctx, []OutputBatch{{SessionID: sessionID, Events: []OutputEvent{event}}}, maxRetained); err != nil {
		return OutputEvent{}, 0, err
	}
	earliest, _, err := s.OutputBounds(ctx, sessionID)
	return event, earliest, err
}

// OutputBatch is one session's part of a group commit: an optional advance past output that was
// never persisted (a replay gap), then contiguous output events.
type OutputBatch struct {
	SessionID              string
	Advance                bool
	AdvanceFrom, AdvanceTo uint64
	Events                 []OutputEvent
}

// AppendOutputBatch persists every session's pending output in one transaction, so the host's
// fsync rate stays bounded however many terminals print at once.
func (s *Store) AppendOutputBatch(ctx context.Context, batches []OutputBatch, maxRetained uint64) (resultErr error) {
	defer protectStoreError(&resultErr)
	if maxRetained == 0 {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer retainRollback(tx, &resultErr)
	for _, batch := range batches {
		if batch.Advance {
			if err := advanceOutputTx(ctx, tx, batch.SessionID, batch.AdvanceFrom, batch.AdvanceTo); err != nil {
				return err
			}
		}
		for _, event := range batch.Events {
			if err := appendOutputTx(ctx, tx, batch.SessionID, event, maxRetained); err != nil {
				return err
			}
		}
	}
	if s.hook != nil {
		if err := s.hook("append_before_commit"); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.hook != nil {
		if err := s.hook("append_after_commit"); err != nil {
			return err
		}
	}
	return nil
}

func appendOutputTx(ctx context.Context, tx *sql.Tx, sessionID string, event OutputEvent, maxRetained uint64) error {
	start, data := event.StartSequence, event.Data
	end := start + uint64(len(data))
	if len(data) == 0 || end < start || event.EndSequence != end {
		return ErrConflict
	}
	var latest, earliest uint64
	if err := tx.QueryRowContext(ctx, `SELECT latest_sequence,earliest_sequence FROM sessions WHERE id=?`, sessionID).Scan(&latest, &earliest); err != nil {
		return err
	}
	if latest != start || earliest > latest {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO output_events(session_id,start_sequence,end_sequence,channel,data) VALUES(?,?,?,?,?)`, sessionID, start, end, event.Channel, append([]byte(nil), data...)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET latest_sequence=?,updated_at=? WHERE id=?`, end, time.Now().UTC().UnixNano(), sessionID); err != nil {
		return err
	}
	// Session bounds are contiguous by construction, so they are the exact
	// retained byte count. Avoid scanning every historical BLOB on each PTY
	// write: that can starve terminal attach behind high-output sessions.
	retained := end - earliest
	for retained > maxRetained {
		var eventStart, eventEnd, size uint64
		if err := tx.QueryRowContext(ctx, `SELECT start_sequence,end_sequence,length(data) FROM output_events WHERE session_id=? ORDER BY start_sequence LIMIT 1`, sessionID).Scan(&eventStart, &eventEnd, &size); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM output_events WHERE session_id=? AND start_sequence=?`, sessionID, eventStart); err != nil {
			return err
		}
		retained -= size
		earliest = eventEnd
	}
	if retained == 0 {
		earliest = end
	}
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET earliest_sequence=? WHERE id=?`, earliest, sessionID)
	return err
}

func (s *Store) Replay(ctx context.Context, sessionID string, from, limit uint64) (outputResults []OutputEvent, earliestResult uint64, latestResult uint64, resultErr error) {
	defer protectStoreError(&resultErr)
	earliest, latest, err := s.OutputBounds(ctx, sessionID)
	if err != nil {
		return nil, 0, 0, err
	}
	if from < earliest {
		return nil, earliest, latest, &GapError{from, earliest, latest}
	}
	if from > latest {
		return nil, earliest, latest, ErrConflict
	}
	rows, err := s.q.ReplayOutput(ctx, storesqlc.ReplayOutputParams{SessionID: sessionID, EndSequence: int64(from)})
	if err != nil {
		return nil, earliest, latest, err
	}
	var events []OutputEvent
	remaining := limit
	for _, row := range rows {
		event := OutputEvent{Channel: byte(row.Channel), StartSequence: uint64(row.StartSequence), EndSequence: uint64(row.EndSequence), Data: row.Data}
		start := max(from, event.StartSequence)
		end := event.EndSequence
		if limit > 0 && end-start > remaining {
			end = start + remaining
		}
		offsetStart := start - event.StartSequence
		offsetEnd := end - event.StartSequence
		if end > start {
			event.StartSequence = start
			event.EndSequence = end
			event.Data = append([]byte(nil), event.Data[offsetStart:offsetEnd]...)
			events = append(events, event)
			if limit > 0 {
				remaining -= end - start
				if remaining == 0 {
					break
				}
			}
		}
	}
	return events, earliest, latest, nil
}

// TrimOutput compacts a session's retained output to maxRetained bytes. It is
// used at startup so a reduced configured history limit takes effect before
// old output is restored into memory or replayed to a newly attached client.
func (s *Store) TrimOutput(ctx context.Context, sessionID string, maxRetained uint64) (sequenceResult uint64, resultErr error) {
	defer protectStoreError(&resultErr)
	if maxRetained == 0 {
		return 0, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer retainRollback(tx, &resultErr)
	var earliest, latest uint64
	if err := tx.QueryRowContext(ctx, `SELECT earliest_sequence,latest_sequence FROM sessions WHERE id=?`, sessionID).Scan(&earliest, &latest); err != nil {
		return 0, err
	}
	retained := latest - earliest
	for retained > maxRetained {
		var start, end, size uint64
		if err := tx.QueryRowContext(ctx, `SELECT start_sequence,end_sequence,length(data) FROM output_events WHERE session_id=? ORDER BY start_sequence LIMIT 1`, sessionID).Scan(&start, &end, &size); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM output_events WHERE session_id=? AND start_sequence=?`, sessionID, start); err != nil {
			return 0, err
		}
		retained -= size
		earliest = end
	}
	if retained == 0 {
		earliest = latest
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET earliest_sequence=? WHERE id=?`, earliest, sessionID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return earliest, nil
}
