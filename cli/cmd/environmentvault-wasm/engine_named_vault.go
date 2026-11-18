package main

import (
	"sort"

	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/shared/envname"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// namedVault is the browser side of named ENV vaults (PLAN F26), with the same code as the CLI:
//   - named-vault-wraps {name, team_id?}: a new vault key wrapped for this account (and team);
//     returns {wraps} for POST /v1/workspaces/{ws}/env-vaults.
//   - named-vault-recipe-wrap {name, vault_wraps, recipe_public_key, token_id}: the vault's key,
//     opened with this account's keys, wrapped to a recipe; returns {vault, wrapped} for a recipe's
//     vault_wraps (create and update).
//   - named-vault-values {name, vault_wraps, key_epoch, values_envelope, variable?, value?, delete?}:
//     opens the vault's values and returns their {names}; with `variable` it sets (or, with `delete`,
//     removes) that one and also returns the new {envelope} for PUT .../env-vaults/{name}/values, as
//     `pb env set --vault` does. Values never leave the engine.
func (e *engine) namedVault(r request) (any, error) {
	if err := e.current(r.Vault); err != nil {
		return nil, err
	}
	if !recipe.ValidName(r.Name) {
		return nil, environmente2ee.ErrInvalid
	}
	account := e.head.AccountID
	if r.Action == "named-vault-wraps" {
		key, err := recipe.NewVaultKey()
		if err != nil {
			return nil, err
		}
		defer clear(key)
		wraps, err := environmente2ee.NamedVaultWraps(&e.keys, account, r.TeamID, r.Name, key)
		if err != nil {
			return nil, err
		}
		return map[string]any{"wraps": wraps}, nil
	}
	if r.Action == "named-vault-values" {
		return e.namedVaultValues(r)
	}
	if !recipe.ValidID(r.TokenID) || len(r.RecipePublicKey) != 32 {
		return nil, environmente2ee.ErrInvalid
	}
	wrapped, err := environmente2ee.WrapNamedVaultForRecipe(&e.keys, account, r.Name, r.VaultWraps, r.RecipePublicKey, r.TokenID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"vault": r.Name, "wrapped": wrapped}, nil
}

func (e *engine) namedVaultValues(r request) (any, error) {
	if r.Variable != "" && !r.Delete {
		if err := envname.Check(r.Variable); err != nil {
			return nil, err
		}
	}
	key, err := environmente2ee.OpenNamedVaultKey(&e.keys, e.head.AccountID, r.Name, r.VaultWraps)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	epoch := int(r.KeyEpoch)
	values, err := recipe.OpenValues(key, r.Name, epoch, r.ValuesEnvelope)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if r.Variable != "" {
		if r.Delete {
			delete(values, r.Variable)
		} else {
			values[r.Variable] = r.Value
		}
		if out["envelope"], err = recipe.SealValues(key, r.Name, epoch, values); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out["names"] = names
	return out, nil
}
