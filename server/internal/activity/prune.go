package activity

import (
	"context"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// PruneBatch bounds one prune transaction.
const PruneBatch = 500

// Prune deletes one bounded batch of events past their workspace plan's retention and reports
// how many it deleted. It is the only deleter: the database refuses any other deletion and any
// deletion of an event younger than the shortest retention (reject_audit_event_mutation).
func (s *Service) Prune(ctx context.Context) (int64, error) {
	var deleted int64
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('paperboat.audit_prune','on',true)`); err != nil {
			return err
		}
		// Everything past the longest retention; then personal workspaces past Pro's; then
		// personal workspaces on Free past Free's. Each uses its own index and is bounded.
		free, pro, team := s.cfg.AuditRetentionFree.Seconds(), s.cfg.AuditRetentionPro.Seconds(), s.cfg.AuditRetentionTeam.Seconds()
		for _, step := range []struct {
			query  string
			cutoff float64
		}{
			{`DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events WHERE created_at < now()-make_interval(secs=>$1)
 ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED)`, team},
			{`DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events WHERE activity_workspace_id LIKE 'personal:%'
 AND created_at < now()-make_interval(secs=>$1) ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED)`, pro},
			{`DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events WHERE activity_workspace_id LIKE 'personal:%'
 AND created_at < now()-make_interval(secs=>$1) AND audit_retention_class(activity_workspace_id)='free'
 ORDER BY created_at, id LIMIT $2 FOR UPDATE SKIP LOCKED)`, free},
		} {
			tag, err := tx.Exec(ctx, step.query, step.cutoff, PruneBatch)
			if err != nil {
				return err
			}
			deleted += tag.RowsAffected()
		}
		return nil
	})
	return deleted, err
}
