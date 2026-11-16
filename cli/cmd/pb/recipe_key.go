package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/recipekey"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
	"github.com/spf13/cobra"
)

// saveRecipeKey writes this machine's recipe key file (internal/recipekey) for a fresh pairing.
func saveRecipeKey(cfg *config.Config, token recipe.Token) error {
	f, err := recipekey.New(token)
	if err != nil {
		return err
	}
	return recipekey.Save(cfg.Path(), f)
}

// recipeVaultEnvironment is NAME=value for every value of the vaults this machine's recipe reads; on
// a machine not paired from a recipe it is empty.
var recipeVaultEnvironment = func(ctx context.Context, command *cobra.Command) ([]string, error) {
	cfg, err := config.Load(configPathFlag(command))
	if err != nil {
		return nil, err
	}
	key, ok, err := recipekey.Load(cfg.Path())
	if err != nil || !ok {
		return nil, err
	}
	client, err := backendClient(actionContext(command, nil))
	if err != nil {
		return nil, err
	}
	state, err := client.RecipeMachine(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the recipe's vaults: %w", friendlyCommandError(err))
	}
	// After a rotate, the recipe's new key arrives sealed to this machine.
	if len(state.MachineKey) > 0 {
		if key, _, err = recipekey.Rotated(cfg.Path(), key, state.MachineKey); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, v := range state.Vaults {
		vaultKey, err := recipe.UnwrapWithPrivate(key.PrivateKey, recipe.RecipeVaultInfo(v.Name, key.TokenID), v.Wrapped)
		if err != nil {
			return nil, fmt.Errorf("open vault %s with this machine's recipe key: %w", v.Name, err)
		}
		values, err := recipe.OpenValues(vaultKey, v.Name, v.KeyEpoch, v.Envelope)
		clear(vaultKey)
		if err != nil {
			return nil, fmt.Errorf("open the values of vault %s: %w", v.Name, err)
		}
		for name, value := range values {
			if envHookName(name) && !strings.ContainsRune(value, 0) {
				out = append(out, name+"="+value)
			}
		}
	}
	return out, nil
}

// withRecipeVaultValues adds the recipe's vault values under names the runtime did not set.
func withRecipeVaultValues(ctx context.Context, command *cobra.Command, values []string) ([]string, error) {
	extra, err := recipeVaultEnvironment(ctx, command)
	if err != nil || len(extra) == 0 {
		return values, err
	}
	seen := map[string]bool{}
	for _, entry := range values {
		name, _, _ := strings.Cut(entry, "=")
		seen[name] = true
	}
	for _, entry := range extra {
		name, _, _ := strings.Cut(entry, "=")
		if !seen[name] {
			values = append(values, entry)
		}
	}
	return values, nil
}
