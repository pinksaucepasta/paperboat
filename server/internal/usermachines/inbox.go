package usermachines

import (
	"context"
	"database/sql"
	"errors"
)

// CanManageInbox says whether the actor may read and delete a machine's Paperboat Inbox: its owner,
// or an owner or admin of the team that owns it, in the selected workspace. File names are private,
// so everyone else (and a machine that does not exist) is ErrNotFound. The Inbox itself is never
// stored here: the machine answers live.
func (s *Service) CanManageInbox(ctx context.Context, userID, userMachineID string) error {
	_, teamID := workspaceTeamArgument(ctx)
	var found bool
	err := s.db.SQL().QueryRowContext(ctx, `SELECT true FROM user_machines m
 WHERE m.id=$1 AND m.deleted_at IS NULL AND machine_management_allowed($2,m.id)
  AND ($3::text IS NULL OR m.owner_team_id=$3)`, userMachineID, userID, teamID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
