package recipes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// MachineRecipe is what a recipe machine applies (GET /v1/recipe-machine).
type MachineRecipe struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Revision int            `json:"revision"`
	Recipe   recipe.Recipe  `json:"recipe"`
	Vaults   []MachineVault `json:"vaults"`
	// MachineKey is the recipe's new private key sealed to this machine after a rotate; empty otherwise.
	MachineKey []byte `json:"machine_key,omitempty"`
}

// MachineRecipe returns the live recipe for a recipe session; machineID is "" before pairing.
func (s *Service) MachineRecipe(ctx context.Context, recipeID, machineID string) (MachineRecipe, error) {
	var out MachineRecipe
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		r, err := getTx(ctx, tx, recipeID, false)
		if err != nil {
			return err
		}
		if r.RevokedAt != nil {
			return ErrNotFound
		}
		out = MachineRecipe{ID: r.ID, Name: r.Name, Revision: r.Revision, Recipe: r.Recipe}
		return nil
	})
	if err != nil {
		return MachineRecipe{}, fmt.Errorf("read machine recipe: %w", err)
	}
	if out.Vaults, err = s.RecipeVaults(ctx, recipeID); err != nil {
		return MachineRecipe{}, err
	}
	if machineID != "" {
		if out.MachineKey, err = s.MachineKey(ctx, recipeID, machineID); err != nil {
			return MachineRecipe{}, err
		}
	}
	return out, nil
}

// RecordApply stores the revision a machine applied and each section's outcome.
func (s *Service) RecordApply(ctx context.Context, recipeID, machineID string, revision int, sections map[string]any, publicKey []byte) error {
	if revision < 1 || len(sections) > 16 || (publicKey != nil && len(publicKey) != 32) {
		return ErrInvalidInput
	}
	state, err := json.Marshal(sections)
	if err != nil || len(state) > 32<<10 {
		return ErrInvalidInput
	}
	tag, err := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.recipe_machines SET applied_revision=$3, apply_state=$4, public_key=coalesce($5, public_key) WHERE machine_id=$1 AND recipe_id=$2`, machineID, recipeID, revision, state, publicKey)
	if err != nil {
		return fmt.Errorf("record recipe apply: %w", err)
	}
	if n, _ := tag.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// publishRecipeTx moves the recipe topic of the live machines of the given recipes, so their daemons
// refetch GET /v1/recipe-machine at once instead of polling.
func publishRecipeTx(ctx context.Context, tx *db.Tx, recipeIDs ...string) error {
	_, err := tx.Exec(ctx, `SELECT paperboat.publish_machine_topic(ARRAY(SELECT rm.machine_id FROM recipe_machines rm
	 JOIN live_user_machines m ON m.id=rm.machine_id WHERE rm.recipe_id=ANY($1::text[])), 'recipe')`, recipeIDs)
	return err
}

// publishVaultTx moves the recipe topic of the machines of every recipe that reads the vault.
func publishVaultTx(ctx context.Context, tx *db.Tx, vaultID string) error {
	_, err := tx.Exec(ctx, `SELECT paperboat.publish_machine_topic(ARRAY(SELECT rm.machine_id FROM recipe_vault_wraps w
	 JOIN recipe_machines rm ON rm.recipe_id=w.recipe_id JOIN live_user_machines m ON m.id=rm.machine_id WHERE w.vault_id=$1), 'recipe')`, vaultID)
	return err
}
