package recipes

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// MachineKey is the recipe's new private key for one of its machines, sealed on the creator's
// computer to that machine's ENV host-recipient key (rotate keeps the machines reading vaults).
type MachineKey struct {
	MachineID string
	Sealed    []byte
}

// Rotate gives the recipe a new token (the client made it: same token id, new auth). The old token
// stops pairing at once; the machines keep running. Without confirm, or with a stale one, it
// returns *ConfirmError with the impact. Rotating again with the same auth returns the recipe.
func (s *Service) Rotate(ctx context.Context, actor string, ws workspace.Scope, name string, auth []byte, keys []MachineKey, confirm string) (Recipe, error) {
	if len(auth) != 32 || len(keys) > 1000 {
		return Recipe{}, ErrInvalidInput
	}
	var out Recipe
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeWriteTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		r, err := byNameTx(ctx, tx, ws, actor, name, true)
		if err != nil {
			return err
		}
		var current []byte
		if err := tx.QueryRow(ctx, `SELECT auth_hash FROM recipes WHERE id=$1`, r.ID).Scan(&current); err != nil {
			return err
		}
		if string(current) == string(recipe.AuthHash(auth)) {
			out = r
			return nil
		}
		impact, err := impactTx(ctx, tx, r)
		if err != nil {
			return err
		}
		if code := confirmCode("rotate", r, impact); confirm != code {
			return &ConfirmError{Code: code, Impact: impact}
		}
		now := s.now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE recipes SET previous_auth_hash=auth_hash, auth_hash=$2, rotated_at=$3, updated_at=$3,
		 expires_at=$4 WHERE id=$1`, r.ID, recipe.AuthHash(auth), now, now.Add(r.Recipe.ExpiresIn())); err != nil {
			return mapInsertError(err)
		}
		for _, k := range keys {
			if len(k.Sealed) == 0 || len(k.Sealed) > 512 {
				return ErrInvalidInput
			}
			tag, err := tx.Exec(ctx, `INSERT INTO recipe_machine_keys(machine_id,recipe_id,sealed,created_at)
			 SELECT rm.machine_id, rm.recipe_id, $3, $4 FROM recipe_machines rm WHERE rm.machine_id=$1 AND rm.recipe_id=$2
			 ON CONFLICT (machine_id) DO UPDATE SET sealed=EXCLUDED.sealed, created_at=EXCLUDED.created_at`, k.MachineID, r.ID, k.Sealed, now)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrInvalidInput
			}
		}
		if out, err = getTx(ctx, tx, r.ID, false); err != nil {
			return err
		}
		if err := publishRecipeTx(ctx, tx, r.ID); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "recipe.rotated",
			ResourceType: "recipe", ResourceID: r.ID, Metadata: map[string]any{"workspace": ws.ID, "name": name, "machines": len(impact.Machines)}})
	})
	if err != nil {
		return Recipe{}, fmt.Errorf("rotate recipe: %w", err)
	}
	return out, nil
}

// Revoke ends the recipe: its token stops working, its sessions end, its vault wraps are deleted
// (the vaults then need a new key before new values are written) and its machines are removed.
func (s *Service) Revoke(ctx context.Context, actor string, ws workspace.Scope, name, confirm string) (Recipe, Impact, error) {
	var out Recipe
	var impact Impact
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeWriteTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		r, err := byNameTx(ctx, tx, ws, actor, name, true)
		if err != nil {
			return err
		}
		if impact, err = impactTx(ctx, tx, r); err != nil {
			return err
		}
		if code := confirmCode("revoke", r, impact); confirm != code {
			return &ConfirmError{Code: code, Impact: impact}
		}
		now := s.now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE recipes SET revoked_at=$2, revoked_by=$3 WHERE id=$1`, r.ID, now, actor); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cli_client_sessions SET state='revoked',revoked_at=$2,revocation_reason='recipe_revoked',version=version+1
		 WHERE recipe_id=$1 AND state='active'`, r.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_named_vaults SET rekey_required=true WHERE id IN (SELECT vault_id FROM recipe_vault_wraps WHERE recipe_id=$1)`, r.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recipe_vault_wraps WHERE recipe_id=$1`, r.ID); err != nil {
			return err
		}
		if out, err = getTx(ctx, tx, r.ID, false); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "recipe.revoked",
			ResourceType: "recipe", ResourceID: r.ID, Metadata: map[string]any{"workspace": ws.ID, "name": name, "machines": len(impact.Machines)}})
	})
	if err != nil {
		return Recipe{}, Impact{}, fmt.Errorf("revoke recipe: %w", err)
	}
	// Each removal is the pb machine revoke path, in its own transaction; a failure leaves the
	// machine for the collector, which removes machines of revoked recipes.
	var firstErr error
	for _, m := range impact.Machines {
		if err := s.remove(ctx, out.AccountID, m.ID); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove machine %s of revoked recipe %s: %w", m.Name, name, err)
		}
	}
	return out, impact, firstErr
}

// impactTx lists the machines and vaults a rotate or revoke touches.
func impactTx(ctx context.Context, tx *db.Tx, r Recipe) (Impact, error) {
	impact := Impact{Machines: []MachineRef{}, Vaults: r.Recipe.Vaults()}
	if impact.Vaults == nil {
		impact.Vaults = []string{}
	}
	rows, err := tx.Query(ctx, `SELECT m.id, m.alias, m.online FROM recipe_machines rm JOIN live_user_machines m ON m.id=rm.machine_id
	 WHERE rm.recipe_id=$1 ORDER BY m.alias LIMIT 1000`, r.ID)
	if err != nil {
		return Impact{}, err
	}
	impact.Machines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (MachineRef, error) {
		var m MachineRef
		return m, row.Scan(&m.ID, &m.Name, &m.Online)
	})
	return impact, err
}
