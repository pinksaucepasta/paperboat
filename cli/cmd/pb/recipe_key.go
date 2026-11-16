package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
	"github.com/spf13/cobra"
)

// A machine paired from a recipe keeps the recipe's token id and X25519 private key (never the
// token's secret) in an owner-only file next to its config, to open the vaults the recipe reads with
// nobody online (PLAN F26 section 6).
type recipeKeyFile struct {
	TokenID    string `json:"token_id"`
	PrivateKey []byte `json:"private_key"`
}

func recipeKeyPath(cfg *config.Config) string {
	return filepath.Join(filepath.Dir(cfg.Path()), "recipe-key.json")
}

func saveRecipeKey(cfg *config.Config, token recipe.Token) error {
	data, err := json.Marshal(recipeKeyFile{TokenID: token.ID, PrivateKey: token.PrivateKey()})
	if err != nil {
		return err
	}
	path := recipeKeyPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("save the recipe key: %w", err)
	}
	return os.Rename(temporary, path)
}

func loadRecipeKey(cfg *config.Config) (recipeKeyFile, bool, error) {
	data, err := os.ReadFile(recipeKeyPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return recipeKeyFile{}, false, nil
	}
	if err != nil {
		return recipeKeyFile{}, false, err
	}
	var key recipeKeyFile
	if err := json.Unmarshal(data, &key); err != nil || !recipe.ValidID(key.TokenID) || len(key.PrivateKey) != 32 {
		return recipeKeyFile{}, false, errors.New("the recipe key file is damaged; run the recipe install again")
	}
	return key, true, nil
}

// recipeVaultEnvironment is NAME=value for every value of the vaults this machine's recipe reads; on
// a machine not paired from a recipe it is empty.
var recipeVaultEnvironment = func(ctx context.Context, command *cobra.Command) ([]string, error) {
	cfg, err := config.Load(configPathFlag(command))
	if err != nil {
		return nil, err
	}
	key, ok, err := loadRecipeKey(cfg)
	if err != nil || !ok {
		return nil, err
	}
	defer clear(key.PrivateKey)
	client, err := backendClient(actionContext(command, nil))
	if err != nil {
		return nil, err
	}
	state, err := client.RecipeMachine(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the recipe's vaults: %w", friendlyCommandError(err))
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
