package recipes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// Named ENV vaults (PLAN F26 section 6). Every key operation happens on a member's computer; the
// server keeps ciphertext, wraps and the epoch/revision counters that order writes.

var (
	ErrVaultNotFound = errors.New("vault not found")
	ErrVaultTaken    = errors.New("vault name taken")
	// ErrVaultStale: the expected revision or key epoch is not the current one.
	ErrVaultStale = errors.New("vault changed; read it again")
	ErrVaultInUse = errors.New("vault is read by a recipe")
)

// Wrap is the vault key wrapped for one member (account) or the team.
type Wrap struct {
	Kind    string `json:"recipient_kind"`
	ID      string `json:"recipient_id"`
	Wrapped []byte `json:"wrapped"`
}

// RecipeWrap is the vault key wrapped to a recipe's public key.
type RecipeWrap struct {
	Recipe  string `json:"recipe"`
	Wrapped []byte `json:"wrapped"`
}

type Vault struct {
	Name     string `json:"name"`
	KeyEpoch int    `json:"key_epoch"`
	Revision int64  `json:"revision"`
	Envelope []byte `json:"envelope,omitempty"`
	// Wraps are the wraps the caller may use (its own account and the team).
	Wraps   []Wrap   `json:"wraps"`
	Recipes []string `json:"recipes"`
	id      string
}

// VaultWrap is a vault key wrapped to the recipe being created or updated.
type VaultWrap struct {
	Vault   string `json:"vault"`
	Wrapped []byte `json:"wrapped"`
}

func checkWraps(ws workspace.Scope, actor string, wraps []Wrap) error {
	if len(wraps) == 0 || len(wraps) > 64 {
		return ErrInvalidInput
	}
	for _, w := range wraps {
		if (w.Kind != "account" && w.Kind != "team") || w.ID == "" || len(w.Wrapped) == 0 || len(w.Wrapped) > 512 {
			return ErrInvalidInput
		}
	}
	// The writer must keep access: its own account wrap, or the team's.
	if !slices.ContainsFunc(wraps, func(w Wrap) bool {
		return (w.Kind == "account" && w.ID == actor) || (w.Kind == "team" && w.ID == ws.TeamID)
	}) {
		return ErrInvalidInput
	}
	return nil
}

// CreateVault makes an empty named vault (epoch 1) with the creator's wraps.
func (s *Service) CreateVault(ctx context.Context, actor string, ws workspace.Scope, name string, wraps []Wrap) (Vault, error) {
	if !recipe.ValidName(name) || checkWraps(ws, actor, wraps) != nil {
		return Vault{}, ErrInvalidInput
	}
	id, err := newID()
	if err != nil {
		return Vault{}, err
	}
	id = "envvault_" + id[len("recipe_"):]
	var out Vault
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeWriteTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_named_vaults(id,owner_account,team_id,name) VALUES($1,$2,nullif($3,''),$4)`, id, actor, ws.TeamID, name); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" {
				return ErrVaultTaken
			}
			return err
		}
		if err := replaceWrapsTx(ctx, tx, id, 1, wraps); err != nil {
			return err
		}
		var err error
		if out, err = vaultTx(ctx, tx, actor, ws, name, false); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "environment.vault.created",
			ResourceType: "environment_vault", ResourceID: id, Metadata: map[string]any{"workspace": ws.ID, "name": name}})
	})
	return out, err
}

// ListVaults lists the workspace's vaults without their values.
func (s *Service) ListVaults(ctx context.Context, actor string, ws workspace.Scope) ([]Vault, error) {
	var out []Vault
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		names, err := vaultNamesTx(ctx, tx, actor, ws)
		if err != nil {
			return err
		}
		for _, n := range names {
			v, err := vaultTx(ctx, tx, actor, ws, n, false)
			if err != nil {
				return err
			}
			v.Envelope = nil
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// GetVault returns one vault with its values envelope and the caller's wraps.
func (s *Service) GetVault(ctx context.Context, actor string, ws workspace.Scope, name string) (Vault, error) {
	var out Vault
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		var err error
		out, err = vaultTx(ctx, tx, actor, ws, name, false)
		return err
	})
	return out, err
}

// PutValues replaces the values envelope, encrypted with the key of keyEpoch.
func (s *Service) PutValues(ctx context.Context, actor string, ws workspace.Scope, name string, expectedRevision int64, keyEpoch int, envelope []byte) (Vault, error) {
	if len(envelope) == 0 || len(envelope) > 1<<20 {
		return Vault{}, ErrInvalidInput
	}
	var out Vault
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeWriteTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		v, err := vaultTx(ctx, tx, actor, ws, name, true)
		if err != nil {
			return err
		}
		if v.Revision != expectedRevision || v.KeyEpoch != keyEpoch {
			return ErrVaultStale
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_named_vaults SET envelope=$2, revision=revision+1, updated_at=now() WHERE id=$1`, v.id, envelope); err != nil {
			return err
		}
		if err := publishVaultTx(ctx, tx, v.id); err != nil {
			return err
		}
		out, err = vaultTx(ctx, tx, actor, ws, name, false)
		return err
	})
	return out, err
}

// Rekey moves the vault to a new key: new envelope, new member wraps, and new wraps for exactly the
// recipes that keep access; any other recipe wrap is dropped.
func (s *Service) Rekey(ctx context.Context, actor string, ws workspace.Scope, name string, expectedEpoch int, envelope []byte, wraps []Wrap, recipeWraps []RecipeWrap) (Vault, error) {
	if len(envelope) > 1<<20 || checkWraps(ws, actor, wraps) != nil || len(recipeWraps) > 256 {
		return Vault{}, ErrInvalidInput
	}
	var out Vault
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeWriteTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		v, err := vaultTx(ctx, tx, actor, ws, name, true)
		if err != nil {
			return err
		}
		if v.KeyEpoch != expectedEpoch {
			return ErrVaultStale
		}
		id, epoch := v.id, expectedEpoch+1
		if _, err := tx.Exec(ctx, `UPDATE environment_named_vaults SET envelope=$2, key_epoch=$3, revision=revision+1, updated_at=now() WHERE id=$1`,
			id, nullIfEmpty(envelope), epoch); err != nil {
			return err
		}
		if err := replaceWrapsTx(ctx, tx, id, epoch, wraps); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recipe_vault_wraps WHERE vault_id=$1`, id); err != nil {
			return err
		}
		for _, w := range recipeWraps {
			if len(w.Wrapped) == 0 || len(w.Wrapped) > 512 {
				return ErrInvalidInput
			}
			tag, err := tx.Exec(ctx, `INSERT INTO recipe_vault_wraps(recipe_id,vault_id,key_epoch,wrapped)
			 SELECT r.id,$2,$3,$4 FROM recipes r WHERE r.name=$1 AND r.revoked_at IS NULL AND coalesce(r.team_id,'')=$5 AND (r.team_id IS NOT NULL OR r.account_id=$6)`,
				w.Recipe, id, epoch, w.Wrapped, ws.TeamID, actor)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrNotFound
			}
		}
		if out, err = vaultTx(ctx, tx, actor, ws, name, false); err != nil {
			return err
		}
		if err := publishVaultTx(ctx, tx, id); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "environment.vault.rekeyed",
			ResourceType: "environment_vault", ResourceID: id, Metadata: map[string]any{"workspace": ws.ID, "name": name, "key_epoch": epoch, "recipes": len(recipeWraps)}})
	})
	return out, err
}

// DeleteVault deletes a vault no live recipe reads.
func (s *Service) DeleteVault(ctx context.Context, actor string, ws workspace.Scope, name string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeWriteTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		v, err := vaultTx(ctx, tx, actor, ws, name, true)
		if err != nil {
			return err
		}
		if len(v.Recipes) > 0 {
			return fmt.Errorf("%w: %v", ErrVaultInUse, v.Recipes)
		}
		id := v.id
		if _, err := tx.Exec(ctx, `UPDATE environment_named_vaults SET deleted_at=now(), envelope=NULL WHERE id=$1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM environment_named_vault_wraps WHERE vault_id=$1`, id); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "environment.vault.deleted",
			ResourceType: "environment_vault", ResourceID: id, Metadata: map[string]any{"workspace": ws.ID, "name": name}})
	})
}

// MachineVault is one vault as a recipe machine reads it: the envelope and the key wrapped to the recipe.
type MachineVault struct {
	Name     string `json:"name"`
	KeyEpoch int    `json:"key_epoch"`
	Revision int64  `json:"revision"`
	Envelope []byte `json:"envelope"`
	Wrapped  []byte `json:"wrapped"`
}

// RecipeVaults returns the vaults a live recipe reads, with nobody online. A vault whose wrap is of
// an older epoch (re-keyed without this recipe) is left out.
func (s *Service) RecipeVaults(ctx context.Context, recipeID string) ([]MachineVault, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT v.name, v.key_epoch, v.revision, coalesce(v.envelope,''::bytea), w.wrapped
	 FROM recipe_vault_wraps w JOIN recipes r ON r.id=w.recipe_id JOIN environment_named_vaults v ON v.id=w.vault_id
	 WHERE w.recipe_id=$1 AND r.revoked_at IS NULL AND v.deleted_at IS NULL AND w.key_epoch=v.key_epoch ORDER BY v.name`, recipeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MachineVault{}
	for rows.Next() {
		var v MachineVault
		if err := rows.Scan(&v.Name, &v.KeyEpoch, &v.Revision, &v.Envelope, &v.Wrapped); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MachineKey returns the sealed recipe key a rotate left for machineID (nil when none).
func (s *Service) MachineKey(ctx context.Context, recipeID, machineID string) ([]byte, error) {
	var sealed []byte
	err := s.db.SQL().QueryRowContext(ctx, `SELECT sealed FROM recipe_machine_keys WHERE machine_id=$1 AND recipe_id=$2`, machineID, recipeID).Scan(&sealed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return sealed, nil
}

// syncRecipeWrapsTx makes the recipe's vault wraps match vaults: a wrap is needed (at the vault's
// current epoch) for each vault it newly reads; a vault it stops reading loses the wrap. No vault
// gets a new key when a recipe stops reading it (user rule 2026-11-16).
func syncRecipeWrapsTx(ctx context.Context, tx *db.Tx, recipeID, account string, ws workspace.Scope, vaults []string, wraps []VaultWrap) error {
	have := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT v.name FROM recipe_vault_wraps w JOIN environment_named_vaults v ON v.id=w.vault_id WHERE w.recipe_id=$1`, recipeID)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, n := range names {
		have[n] = true
		if !slices.Contains(vaults, n) {
			if _, err := tx.Exec(ctx, `DELETE FROM recipe_vault_wraps w USING environment_named_vaults v
			 WHERE w.vault_id=v.id AND w.recipe_id=$1 AND v.name=$2`, recipeID, n); err != nil {
				return err
			}
		}
	}
	for _, v := range vaults {
		i := slices.IndexFunc(wraps, func(w VaultWrap) bool { return w.Vault == v })
		if have[v] && i < 0 {
			continue
		}
		if i < 0 {
			return &WrapMissingError{Vault: v}
		}
		w := wraps[i]
		if len(w.Wrapped) == 0 || len(w.Wrapped) > 512 {
			return ErrInvalidInput
		}
		if _, err := tx.Exec(ctx, `INSERT INTO recipe_vault_wraps(recipe_id,vault_id,key_epoch,wrapped)
		 SELECT $1, v.id, v.key_epoch, $3 FROM environment_named_vaults v WHERE v.name=$2 AND v.deleted_at IS NULL
		  AND coalesce(v.team_id,'')=$4 AND (v.team_id IS NOT NULL OR v.owner_account=$5)
		 ON CONFLICT (recipe_id, vault_id) DO UPDATE SET key_epoch=EXCLUDED.key_epoch, wrapped=EXCLUDED.wrapped`,
			recipeID, v, w.Wrapped, ws.TeamID, account); err != nil {
			return err
		}
	}
	return nil
}

// WrapMissingError means the recipe reads a vault but the request carries no key wrapped for it (the
// client's ENV vault was locked or skipped it).
type WrapMissingError struct{ Vault string }

func (e *WrapMissingError) Error() string {
	return fmt.Sprintf("the recipe reads vault %s but no key was wrapped for it; unlock your ENV vault (pb env unlock) and run the command again", e.Vault)
}

func replaceWrapsTx(ctx context.Context, tx *db.Tx, vaultID string, epoch int, wraps []Wrap) error {
	if _, err := tx.Exec(ctx, `DELETE FROM environment_named_vault_wraps WHERE vault_id=$1`, vaultID); err != nil {
		return err
	}
	for _, w := range wraps {
		if _, err := tx.Exec(ctx, `INSERT INTO environment_named_vault_wraps(vault_id,recipient_kind,recipient_id,key_epoch,wrapped) VALUES($1,$2,$3,$4,$5)
		 ON CONFLICT (vault_id,recipient_kind,recipient_id) DO UPDATE SET key_epoch=EXCLUDED.key_epoch, wrapped=EXCLUDED.wrapped`,
			vaultID, w.Kind, w.ID, epoch, w.Wrapped); err != nil {
			return err
		}
	}
	return nil
}

func vaultTx(ctx context.Context, tx *db.Tx, actor string, ws workspace.Scope, name string, lock bool) (Vault, error) {
	q := `SELECT id, name, key_epoch, revision, coalesce(envelope,''::bytea) FROM environment_named_vaults
	 WHERE deleted_at IS NULL AND name=$1 AND coalesce(team_id,'')=$2 AND (team_id IS NOT NULL OR owner_account=$3)`
	if lock {
		q += ` FOR UPDATE`
	}
	var v Vault
	err := tx.QueryRow(ctx, q, name, ws.TeamID, actor).Scan(&v.id, &v.Name, &v.KeyEpoch, &v.Revision, &v.Envelope)
	if errors.Is(err, pgx.ErrNoRows) {
		return Vault{}, ErrVaultNotFound
	}
	if err != nil {
		return Vault{}, err
	}
	rows, err := tx.Query(ctx, `SELECT recipient_kind, recipient_id, wrapped FROM environment_named_vault_wraps
	 WHERE vault_id=$1 AND key_epoch=$2 AND ((recipient_kind='account' AND recipient_id=$3) OR (recipient_kind='team' AND recipient_id=$4)) ORDER BY 1,2`,
		v.id, v.KeyEpoch, actor, ws.TeamID)
	if err != nil {
		return Vault{}, err
	}
	if v.Wraps, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Wrap, error) {
		var w Wrap
		return w, row.Scan(&w.Kind, &w.ID, &w.Wrapped)
	}); err != nil {
		return Vault{}, err
	}
	rows, err = tx.Query(ctx, `SELECT r.name FROM recipe_vault_wraps w JOIN recipes r ON r.id=w.recipe_id WHERE w.vault_id=$1 AND r.revoked_at IS NULL ORDER BY r.name`, v.id)
	if err != nil {
		return Vault{}, err
	}
	v.Recipes, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if v.Recipes == nil {
		v.Recipes = []string{}
	}
	return v, err
}

func nullIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
