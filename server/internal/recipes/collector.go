package recipes

import (
	"context"
	"errors"
	"fmt"
)

// collectBatch bounds one collector pass.
const collectBatch = 100

// CollectMachines removes recipe machines that are due, as Headscale's ephemeral collector does
// (headscale/hscontrol/app.go): remove_after_offline counts from when a machine was last seen
// and is cancelled while it is online, so a live machine is never removed for being offline;
// machine_lifetime (remove_at) removes it either way; a revoked recipe's machines go too. Removal
// is the pb machine revoke path. A periodic worker calls it every minute.
func (s *Service) CollectMachines(ctx context.Context) error {
	db := s.db.SQL()
	if _, err := db.ExecContext(ctx, `UPDATE paperboat.recipe_machines rm SET offline_remove_at=coalesce(m.last_seen_at, now()) + r.remove_after_offline
	 FROM paperboat.user_machines m, paperboat.recipes r
	 WHERE rm.machine_id=m.id AND r.id=rm.recipe_id AND r.remove_after_offline IS NOT NULL AND NOT m.online AND rm.offline_remove_at IS NULL`); err != nil {
		return fmt.Errorf("schedule offline recipe machines: %w", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE paperboat.recipe_machines rm SET offline_remove_at=NULL
	 FROM paperboat.user_machines m WHERE rm.machine_id=m.id AND m.online AND rm.offline_remove_at IS NOT NULL`); err != nil {
		return fmt.Errorf("cancel removal of online recipe machines: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.user_id FROM paperboat.recipe_machines rm
	 JOIN paperboat.live_user_machines m ON m.id=rm.machine_id JOIN paperboat.recipes r ON r.id=rm.recipe_id
	 WHERE rm.remove_at <= now() OR (rm.offline_remove_at <= now() AND NOT m.online) OR r.revoked_at IS NOT NULL
	 ORDER BY rm.machine_id LIMIT $1`, collectBatch)
	if err != nil {
		return fmt.Errorf("list due recipe machines: %w", err)
	}
	type due struct{ machine, account string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.machine, &d.account); err != nil {
			rows.Close()
			return err
		}
		list = append(list, d)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var errs []error
	for _, d := range list {
		if err := s.remove(ctx, d.account, d.machine); err != nil {
			errs = append(errs, fmt.Errorf("remove recipe machine %s: %w", d.machine, err))
		}
	}
	return errors.Join(errs...)
}
