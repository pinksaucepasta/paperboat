package recordings

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// ApplyReportTx updates the index from one machine report (already validated with Report.Validate) inside the
// audit backlog's transaction. Every step is idempotent, because the backlog may be uploaded again. Only the
// reporting machine's own rows change. A recording reported by a machine that has no entitled recording team (the team
// that owns it or that it is shared into) is not
// indexed (its audit event still is).
func ApplyReportTx(ctx context.Context, tx *db.Tx, machineID, kind string, r recording.Report) error {
	switch kind {
	case recording.KindStarted:
		var team, name string
		err := tx.QueryRow(ctx, `SELECT paperboat.recording_team(m.id), m.alias FROM user_machines m JOIN teams t ON t.team_id = paperboat.recording_team(m.id)
 WHERE m.id = $1 AND t.deleted_at IS NULL AND team_paid_entitled(t.team_id)`, machineID).Scan(&team, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var startedBy *string
		if r.ActorAccountID != "" {
			startedBy = &r.ActorAccountID
		}
		_, err = tx.Exec(ctx, `INSERT INTO session_recordings(id, team_id, machine_id, machine_name, session_id, trigger, started_by, key_generation, accounts, started_at)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT (id) DO NOTHING`,
			r.RecordingID, team, machineID, name, r.SessionID, r.Trigger, startedBy, r.KeyGeneration, nonNil(r.Accounts), r.StartedAt)
		return err
	case recording.KindStopped:
		_, err := tx.Exec(ctx, `UPDATE session_recordings SET state = 'ended', ended_at = $3, end_reason = $4, bytes = $5, segments = $6,
 accounts = ARRAY(SELECT DISTINCT a FROM unnest(accounts || $7::text[]) a ORDER BY a LIMIT 64)
 WHERE id = $1 AND machine_id = $2 AND state = 'recording'`, r.RecordingID, machineID, r.EndedAt, r.Reason, r.Bytes, r.Segments, nonNil(r.Accounts))
		return err
	case recording.KindDeleted:
		_, err := tx.Exec(ctx, `UPDATE session_recordings SET state = 'deleted', deleted_at = now() WHERE id = $1 AND machine_id = $2 AND state <> 'deleted'`, r.RecordingID, machineID)
		return err
	}
	return recording.ErrInvalidReport
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
