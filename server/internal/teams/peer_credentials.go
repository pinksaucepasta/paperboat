package teams

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// revokePeerCredentialsFirst runs the part of removing team machines that bumps other machines' peer identities (their
// CLI sessions and peer authority, db.RevokeMachinePeerCredentialsTx) in a short READ COMMITTED transaction before the
// serializable action: there those bumps lost every retry to the bumped machines' configuration requests (40001), here
// they wait for the writers' row locks. machine "" means every live machine the team owns (team delete). Invariants:
//   - only an action that would be allowed revokes anything: the checks mutate makes (team not suspended, actor a current
//     member at the expected generation, SSO) and the action's own rule (allowed, the same function the action calls),
//     read on committed state under the team's row lock; otherwise nothing happens here and the action reports the
//     error. The serializable action re-checks all of them and stays the authority;
//   - ordering with the team's other writes: the membership session lock and the team lock, as mutate takes them;
//   - every active CLI session and unrevoked certificate of the machines ends: the UPDATEs re-check their rows under the
//     machine row locks; one created after this step is revoked by the action's own cleanup (the same queries);
//   - the identity bumps are increments on locked rows, with no read-then-write.
//
// If the action then fails (a concurrent change moved the generation), the credentials stay revoked: the safe side,
// and the retry completes the action.
func (s *Service) revokePeerCredentialsFirst(ctx context.Context, account, team string, expected uint64, machine string, allowed func(context.Context, *db.Tx, Team, Member) error) error {
	if !validID(team) {
		return nil
	}
	return s.db.InReadCommittedTxUnderSessionLock(ctx, membershipLockKey, membershipLockSeed, func(ctx context.Context, tx *db.Tx) error {
		if err := LockTeam(ctx, tx, team); err != nil {
			return err
		}
		t, err := ReadTx(ctx, tx, team)
		if err != nil || t.Suspended {
			return nil
		}
		actor, err := check(t, account, expected)
		if err != nil || SSOGuardTx(ctx, tx, team, account) != nil || allowed(ctx, tx, t, actor) != nil {
			return nil
		}
		return RevokeTeamMachinesPeerCredentialsTx(ctx, tx, team, machine)
	})
}

// RevokeTeamMachinesPeerCredentialsTx locks the team's live machines (or the one named) and ends their CLI sessions and
// peer authority (db.RevokeMachinePeerCredentialsTx). Account deletion runs it for the teams it deletes.
func RevokeTeamMachinesPeerCredentialsTx(ctx context.Context, tx *db.Tx, team, machine string) error {
	rows, err := tx.Query(ctx, `SELECT id,user_id FROM user_machines WHERE owner_team_id=$1 AND ($2='' OR id=$2) AND deleted_at IS NULL AND revoked_at IS NULL ORDER BY id FOR UPDATE`, team, machine)
	if err != nil {
		return err
	}
	type owned struct{ ID, User string }
	machines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[owned])
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, m := range machines {
		if err := db.RevokeMachinePeerCredentialsTx(ctx, tx, m.User, m.ID, now); err != nil {
			return err
		}
	}
	return nil
}
