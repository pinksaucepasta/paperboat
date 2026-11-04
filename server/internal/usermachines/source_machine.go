package usermachines

import (
	"context"
	"database/sql"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
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

// descriptorSourceAllowed accepts either an enrolled source or a token's own CLI peer endpoint.
func (s *Service) descriptorSourceAllowed(ctx context.Context, user, source, session, target, capability string) error {
	if tokenID, token := auth.APITokenFromContext(ctx); token {
		if err := planlimits.AllowSDKRequest(user, tokenID, "descriptors"); err != nil {
			return err
		}
		if source == "" {
			return auth.AuthorizeAPITokenSession(ctx, s.db.SQL(), user, session, target, capability, 0)
		}
		// A token session names a source only from the paired machine it signed in on (F18-06); an SDK
		// session is bound to no machine, so a claimed source stays refused (F24-04).
		var bound bool
		if err := s.db.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cli_client_sessions WHERE id=$1 AND user_id=$2 AND state='active' AND user_machine_id=$3)`, session, user, source).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return auth.ErrOutsideTokenScope
		}
	}
	if source == "" || session == "" {
		return ErrNotFound
	}
	allowed, err := s.sourceMachineAllowed(ctx, user, source)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrNotFound
	}
	return nil
}
