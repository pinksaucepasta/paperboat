package main

import (
	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// namedVault is the browser side of named ENV vaults (PLAN F26), with the same code as the CLI:
//   - named-vault-wraps {name, team_id?}: a new vault key wrapped for this account (and team);
//     returns {wraps} for POST /v1/workspaces/{ws}/env-vaults.
//   - named-vault-recipe-wrap {name, vault_wraps, recipe_public_key, token_id}: the vault's key,
//     opened with this account's keys, wrapped to a recipe; returns {vault, wrapped} for a recipe's
//     vault_wraps (create and update).
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
	if !recipe.ValidID(r.TokenID) || len(r.RecipePublicKey) != 32 {
		return nil, environmente2ee.ErrInvalid
	}
	wrapped, err := environmente2ee.WrapNamedVaultForRecipe(&e.keys, account, r.Name, r.VaultWraps, r.RecipePublicKey, r.TokenID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"vault": r.Name, "wrapped": wrapped}, nil
}
