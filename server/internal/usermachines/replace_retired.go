package usermachines

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/environment"
	"github.com/pinksaucepasta/paperboat-server/internal/machinealias"
)

// ReplaceRetiredMachineTx removes the disconnected or revoked record of a computer that was
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
	if retired.State != "disconnected" && retired.State != "revoked" {
		return nil // a live machine is never removed
	}
	replacement, err := tx.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: replacementID, UserID: userID})
	if err != nil {
		return err
	}
	if retired.OwnerTeamID.Valid != replacement.OwnerTeamID.Valid {
		return nil // a personal record never replaces a team one
	}
	if err := s.revokeUserMachineControlTx(ctx, tx, userID, retired, true); err != nil {
		return err
	}
	if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.deleted", ResourceType: "user_machine", ResourceID: retired.ID, IdempotencyKey: "user_machine.deleted:" + retired.ID, Metadata: map[string]any{"replaced_by": replacement.ID}}); err != nil {
		return err
	}
	if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "user_machine.replaced", ResourceType: "user_machine", ResourceID: replacement.ID, IdempotencyKey: "user_machine.replaced:" + retired.ID + ":" + replacement.ID, Metadata: map[string]any{"replaced_machine_id": retired.ID, "alias": retired.Alias}}); err != nil {
		return err
	}
	if replacement.Alias != retired.Alias && isNumberedVariant(replacement.Alias, retired.Alias) {
		if _, err := tx.Queries().RenameUserMachine(ctx, dbsqlc.RenameUserMachineParams{Alias: retired.Alias, Description: replacement.Description, ID: replacement.ID, UserID: userID}); err != nil {
			return err
		}
	}
	return nil
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
