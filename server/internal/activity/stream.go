package activity

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Since returns up to limit events of one workspace after the cursor sequence, oldest first, for
// streaming to a SIEM (PLAN F19-10). Events newer than settle are left for a later read: a
// transaction can commit an event after one with a higher sequence, and a stream cursor must
// never pass an event that is not visible yet.
func (s *Service) Since(ctx context.Context, ws string, after int64, limit int, settle time.Duration) ([]Event, error) {
	var out []Event
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id,created_at,event_type,actor_type,coalesce(actor_user_id,''),coalesce(actor_email,''),coalesce(actor_token_id,''),
  resource_type,resource_id,coalesce(activity_target_name,''),coalesce(activity_machine_id,''),coalesce(activity_machine_name,''),
  result,coalesce(reason,''),coalesce(client_ip,''),coalesce(user_agent,''),metadata,cursor_sequence
 FROM audit_events WHERE activity_workspace_id=$1 AND cursor_sequence>$2 AND created_at<now()-make_interval(secs=>$3)
 ORDER BY cursor_sequence LIMIT $4`, ws, after, settle.Seconds(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := decode(rows, ws)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Sequence is the event's position in the audit log, used as a stream cursor.
func (e Event) Sequence() int64 { return e.sequence }

// Latest is the current end of the audit log; a new stream starts there.
func (s *Service) Latest(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.SQL().QueryRowContext(ctx, `SELECT coalesce(max(cursor_sequence),0) FROM paperboat.audit_events`).Scan(&n)
	return n, err
}
