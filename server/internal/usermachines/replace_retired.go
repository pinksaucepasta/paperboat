package usermachines

import (
	"context"

	"database/sql"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/environment"
	"github.com/pinksaucepasta/paperboat/server/internal/machinealias"
)

// ReplaceRetiredMachineTx removes the disconnected, revoked or offline record of a computer that was
// added again. The caller has already proved it is the same computer (the new machine reports
// SSH host keys the retired record owned) and that both records belong to userID.
//
// The removal is the normal machine removal: the ENV fence, peer authority, host-key claims,
// sessions and control credentials of the retired record are released, and its audit history
// stays (the row is soft-deleted and audit events are separate). The new machine then takes the
// exact name the retired record held when it only got a numbered variant of it.
func (s *Service) ReplaceRetiredMachineTx(ctx context.Context, tx *db.Tx, userID, retiredID, replacementID string) error {
	_, err := s.replaceMachineTx(ctx, tx, userID, retiredID, replacementID, true)
	return err
}

// ErrReplacementTeamMismatch refuses a replacement across ownership: a personal record never replaces a team one.
var ErrReplacementTeamMismatch = errors.New("a personal machine record cannot replace a team one")

// ReplaceMachine is the cutover of pb reset: pb pair calls it with the new machine's session once the new
// runtime is verified (signed configuration fetched, daemon bound), and only then is the old record of the
// same computer removed. A reset or pairing that fails earlier leaves the old record untouched
// (make-before-break). The replacement takes the old record's local address and exact name in the same
// transaction. The caller is the account owner, who may remove the old record anyway, so its state is not
// checked; an old record that is already gone is a no-op, so a retry after a lost answer succeeds.
// Prior art: Tailscale RegisterRequest.OldNodeKey (tailcfg/tailcfg.go) re-keys the same node, which keeps its address.
func (s *Service) ReplaceMachine(ctx context.Context, userID, retiredID, replacementID string) error {
	retiredID, replacementID = strings.TrimSpace(retiredID), strings.TrimSpace(replacementID)
	if retiredID == "" || replacementID == "" || retiredID == replacementID {
		return ErrNotFound
	}
	var replaced bool
	if err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		replaced, err = s.replaceMachineTx(ctx, tx, userID, retiredID, replacementID, false)
		return err
	}); err != nil || !replaced {
		return err
	}
	return s.RevokeUserMachineSessions(ctx, retiredID, "user_machine_deleted")
}

// replaceMachineTx removes retiredID in favour of replacementID. onlyRetired keeps a live record (the
// implicit host-key path); it reports whether a record was removed.
func (s *Service) replaceMachineTx(ctx context.Context, tx *db.Tx, userID, retiredID, replacementID string, onlyRetired bool) (bool, error) {
	if retiredID == "" || replacementID == "" || retiredID == replacementID {
		return false, nil
	}
	if err := environment.LockVaultMutationsTx(ctx, tx); err != nil {
		return false, err
	}
	if err := tx.Queries().LockUserMachineAliases(ctx, userID); err != nil {
		return false, err
	}
	retired, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: retiredID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // already removed, or not this account's record
	}
	if err != nil {
		return false, err
	}
	// Only a record that cannot answer is replaced: removed/revoked/disconnected, or offline right now.
	// A live machine is never removed.
	if onlyRetired && retired.State != "disconnected" && retired.State != "revoked" && !(retired.State == "offline" && !retired.Online) {
		return false, nil
	}
	replacement, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: replacementID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) && !onlyRetired {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if retired.OwnerTeamID.Valid != replacement.OwnerTeamID.Valid {
		if onlyRetired {
			return false, nil
		}
		return false, ErrReplacementTeamMismatch
	}
	if err := s.revokeUserMachineControlTx(ctx, tx, userID, retired, true); err != nil {
		return false, err
	}
	if err := s.auditReplacedTx(ctx, tx, userID, retired, replacement.ID); err != nil {
		return false, err
	}
	if err := adoptControlSyncLease(ctx, tx, retired.ID, replacement.ID); err != nil {
		return false, err
	}
	// The explicit cutover (pb pair after a reset) keeps the exact name whatever the new record was called; the
	// implicit host-key path only reclaims a name the new record got as a numbered variant of it.
	if replacement.Alias != retired.Alias && (!onlyRetired || isNumberedVariant(replacement.Alias, retired.Alias)) {
		if _, err := tx.Queries().RenameUserMachine(ctx, dbsqlc.RenameUserMachineParams{Alias: retired.Alias, ID: replacement.ID, UserID: userID}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// adoptControlSyncLease gives the replacement the retired record's local address lease (the
// 127.100.x.y slot users paste into connection strings), so a computer keeps its address across
// a reset or reinstall. The replacement's own, freshly allocated lease is released.
func adoptControlSyncLease(ctx context.Context, tx *db.Tx, retiredID, replacementID string) error {
	// Leases are keyed by (account, machine): the replacement's own leases in the accounts that held the retired
	// record go, and the retired record's leases move to it, so no account's address changes and no other
	// account's lease with the same slot is touched.
	// The retired record's leases are locked first, as concurrent recoveries of one machine take the same path;
	// without any, the replacement keeps its own.
	var held int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM paperboat.control_sync_leases WHERE machine_id=$1 FOR UPDATE) locked`, retiredID).Scan(&held); err != nil {
		return err
	}
	if held == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM paperboat.control_sync_leases r WHERE r.machine_id=$2
 AND EXISTS(SELECT 1 FROM paperboat.control_sync_leases o WHERE o.machine_id=$1 AND o.account_id=r.account_id)`, retiredID, replacementID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE paperboat.control_sync_leases SET machine_id=$2 WHERE machine_id=$1`, retiredID, replacementID)
	return err
}

// isNumberedVariant reports whether alias is name with the "-N" suffix a fresh enrollment gets
// when name was already taken.
func isNumberedVariant(alias, name string) bool {
	for ordinal := 2; ordinal <= 1000; ordinal++ {
		if machinealias.Candidate(name, ordinal) == alias {
			return true
		}
	}
	return false
}

func (s *Service) auditReplacedTx(ctx context.Context, tx *db.Tx, userID string, retired dbsqlc.UserMachine, replacementID string) error {
	return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.replaced", ResourceType: "user_machine", ResourceID: replacementID, IdempotencyKey: "user_machine.replaced:" + retired.ID + ":" + replacementID, Metadata: map[string]any{"replaced_machine_id": retired.ID, "alias": retired.Alias}})
}

// retireForFreshEnrollmentTx honours a pairing's replaces_machine_id hint when a fresh machine is
// about to be created and the old record is already retired (removed in the dashboard, or revoked).
// A record that may still work is replaced only after the new machine is verified (ReplaceMachine).
// The hint is a request, never authority: only a retired (disconnected, revoked or already removed)
// personal record of the same account is acted on; anything else, including live machines, other
// accounts' records and unknown ids, is ignored. A record that is not yet removed is removed first,
// which frees its exact name. It returns the retired id whose local address the new machine adopts.
func (s *Service) retireForFreshEnrollmentTx(ctx context.Context, tx *db.Tx, userID, hint, newID string) (string, error) {
	hint = strings.TrimSpace(hint)
	if hint == "" || hint == newID {
		return "", nil
	}
	var state string
	var team sql.NullString
	var removed bool
	err := tx.QueryRow(ctx, `SELECT state,owner_team_id,deleted_at IS NOT NULL FROM paperboat.user_machines WHERE id=$1 AND user_id=$2 FOR UPDATE`, hint, userID).Scan(&state, &team, &removed)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if team.Valid {
		return "", nil
	}
	if removed {
		return hint, nil
	}
	if state != "disconnected" && state != "revoked" {
		return "", nil
	}
	if err := environment.LockVaultMutationsTx(ctx, tx); err != nil {
		return "", err
	}
	if err := tx.Queries().LockUserMachineAliases(ctx, userID); err != nil {
		return "", err
	}
	retired, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: hint, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := s.revokeUserMachineControlTx(ctx, tx, userID, retired, true); err != nil {
		return "", err
	}
	return hint, nil
}

// finishFreshReplacementTx runs once the replacement exists: it takes over the retired record's
// local address and records the replacement in the audit history.
func (s *Service) finishFreshReplacementTx(ctx context.Context, tx *db.Tx, userID, retiredID string, replacement dbsqlc.UserMachine) error {
	if err := adoptControlSyncLease(ctx, tx, retiredID, replacement.ID); err != nil {
		return err
	}
	return s.auditReplacedTx(ctx, tx, userID, dbsqlc.UserMachine{ID: retiredID, Alias: replacement.Alias}, replacement.ID)
}
