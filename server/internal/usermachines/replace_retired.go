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
	if retiredID == "" || replacementID == "" || retiredID == replacementID {
		return nil
	}
	if err := environment.LockVaultMutationsTx(ctx, tx); err != nil {
		return err
	}
	if err := tx.Queries().LockUserMachineAliases(ctx, userID); err != nil {
		return err
	}
	retired, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: retiredID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already removed, or not this account's record
	}
	if err != nil {
		return err
	}
	// Only a record that cannot answer is replaced: removed/revoked/disconnected, or offline right now.
	// A live machine is never removed.
	if retired.State != "disconnected" && retired.State != "revoked" && !(retired.State == "offline" && !retired.Online) {
		return nil
	}
	replacement, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: replacementID, UserID: userID})
	if err != nil {
		return err
	}
	if retired.OwnerTeamID.Valid != replacement.OwnerTeamID.Valid {
		return nil // a personal record never replaces a team one
	}
	if err := s.removeRetiredMachineTx(ctx, tx, userID, retired, replacement.ID); err != nil {
		return err
	}
	if err := s.auditReplacedTx(ctx, tx, userID, retired, replacement.ID); err != nil {
		return err
	}
	if err := adoptControlSyncLease(ctx, tx, retired.ID, replacement.ID); err != nil {
		return err
	}
	if replacement.Alias != retired.Alias && isNumberedVariant(replacement.Alias, retired.Alias) {
		if _, err := tx.Queries().RenameUserMachine(ctx, dbsqlc.RenameUserMachineParams{Alias: retired.Alias, ID: replacement.ID, UserID: userID}); err != nil {
			return err
		}
	}
	return nil
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

// removeRetiredMachineTx removes a retired record through the normal removal: ENV fence, peer
// authority, host-key claims, sessions and control credentials are released and its audit
// history stays (the row is soft-deleted).
func (s *Service) removeRetiredMachineTx(ctx context.Context, tx *db.Tx, userID string, retired dbsqlc.UserMachine, replacementID string) error {
	if err := s.revokeUserMachineControlTx(ctx, tx, userID, retired, true); err != nil {
		return err
	}
	return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.deleted", ResourceType: "user_machine", ResourceID: retired.ID, IdempotencyKey: "user_machine.deleted:" + retired.ID, Metadata: map[string]any{"replaced_by": replacementID}})
}

func (s *Service) auditReplacedTx(ctx context.Context, tx *db.Tx, userID string, retired dbsqlc.UserMachine, replacementID string) error {
	return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.replaced", ResourceType: "user_machine", ResourceID: replacementID, IdempotencyKey: "user_machine.replaced:" + retired.ID + ":" + replacementID, Metadata: map[string]any{"replaced_machine_id": retired.ID, "alias": retired.Alias}})
}

// retireForFreshEnrollmentTx honours a pairing's replaces_machine_id hint when a fresh machine is
// about to be created (start fresh deletes the old record first; the new install arrives later).
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
	if err := s.removeRetiredMachineTx(ctx, tx, userID, retired, newID); err != nil {
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
