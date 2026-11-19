package usermachines

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

// A console browser terminal is carried by the edge as it is: its traffic is neither counted against the workspace's edge allowance
// nor slowed down (it is tiny, and a slowed terminal is a broken product). Only the plan's open browser terminals limit applies, also
// to a workspace on its own edges.

// CheckBrowserTerminalLimit refuses opening one more console browser terminal past the plan's
// open browser terminals limit of the machine's workspace (planlimits).
func (s *Service) CheckBrowserTerminalLimit(ctx context.Context, machineID, terminalSessionID string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var scope planlimits.Scope
		err := tx.QueryRow(ctx, `SELECT user_id,coalesce(owner_team_id,'') FROM paperboat.user_machines WHERE id=$1 AND deleted_at IS NULL`, machineID).Scan(&scope.Account, &scope.Team)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the caller's own lookup answers the missing machine
		}
		if err != nil {
			return err
		}
		return planlimits.CheckBrowserTerminalTx(ctx, tx, scope, terminalSessionID)
	})
}
