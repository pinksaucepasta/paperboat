package usermachines

import (
	"context"
	"database/sql"
)

// sourceMachineAllowed reports whether the user may act from this machine: the machine the CLI runs on
// when it asks for a descriptor. It is the user's own personal machine, or a machine owned by a team the
// user is an active member of (a team member's laptop or a shared team host). What the user may do on
// the target is decided separately by the target's capability check. A revoked machine is never a source.
func (s *Service) sourceMachineAllowed(ctx context.Context, userID, machineID string) (bool, error) {
	var allowed bool
	err := s.db.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_machines m WHERE m.id=$1 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.state NOT IN ('revoked','deleted') AND (
 (m.owner_team_id IS NULL AND m.user_id=$2)
 OR EXISTS(SELECT 1 FROM team_members tm JOIN teams t ON t.team_id=tm.team_id
  WHERE tm.team_id=m.owner_team_id AND tm.account_id=$2 AND tm.active AND t.deleted_at IS NULL AND NOT t.suspended)))`,
		machineID, userID).Scan(&allowed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return allowed, err
}
