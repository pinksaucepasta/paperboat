package api

import (
	"context"
	"net/http"
	"net/url"
)

// Named ENV vaults (shared/recipe/README.md "API"). Every key operation happens on this computer;
// the server keeps ciphertext, wraps and the counters that order writes.

type EnvVaultWrap struct {
	Kind    string `json:"recipient_kind"` // account or team
	ID      string `json:"recipient_id"`
	Wrapped []byte `json:"wrapped"`
}

type EnvVaultRecipeWrap struct {
	Recipe  string `json:"recipe"`
	Wrapped []byte `json:"wrapped"`
}

type EnvVault struct {
	Name     string         `json:"name"`
	KeyEpoch int            `json:"key_epoch"`
	Revision int64          `json:"revision"`
	Envelope []byte         `json:"envelope,omitempty"`
	Wraps    []EnvVaultWrap `json:"wraps"`
	Recipes  []string       `json:"recipes"`
}

func envVaultPath(ws, name string) string {
	return workspacePath(ws, "/env-vaults/"+url.PathEscape(name))
}

func (c *Client) EnvVaults(ctx context.Context, ws string) ([]EnvVault, error) {
	var out struct {
		Vaults []EnvVault `json:"vaults"`
	}
	err := c.do(ctx, http.MethodGet, workspacePath(ws, "/env-vaults"), nil, &out)
	return out.Vaults, err
}

func (c *Client) CreateEnvVault(ctx context.Context, ws, name string, wraps []EnvVaultWrap) (EnvVault, error) {
	var out struct {
		Vault EnvVault `json:"vault"`
	}
	err := c.do(ctx, http.MethodPost, workspacePath(ws, "/env-vaults"), map[string]any{"name": name, "wraps": wraps}, &out)
	return out.Vault, err
}

// GetEnvVault returns the vault with its envelope and the wraps the caller may open.
func (c *Client) GetEnvVault(ctx context.Context, ws, name string) (EnvVault, error) {
	var out struct {
		Vault EnvVault `json:"vault"`
	}
	err := c.do(ctx, http.MethodGet, envVaultPath(ws, name), nil, &out)
	return out.Vault, err
}

func (c *Client) PutEnvVaultValues(ctx context.Context, ws, name string, expectedRevision int64, keyEpoch int, envelope []byte) (EnvVault, error) {
	var out struct {
		Vault EnvVault `json:"vault"`
	}
	err := c.do(ctx, http.MethodPut, envVaultPath(ws, name)+"/values", map[string]any{"expected_revision": expectedRevision, "key_epoch": keyEpoch, "envelope": envelope}, &out)
	return out.Vault, err
}

func (c *Client) RekeyEnvVault(ctx context.Context, ws, name string, expectedKeyEpoch int, envelope []byte, wraps []EnvVaultWrap, recipeWraps []EnvVaultRecipeWrap) (EnvVault, error) {
	var out struct {
		Vault EnvVault `json:"vault"`
	}
	err := c.do(ctx, http.MethodPost, envVaultPath(ws, name)+"/rekey", map[string]any{"expected_key_epoch": expectedKeyEpoch, "envelope": envelope, "wraps": wraps, "recipe_wraps": recipeWraps}, &out)
	return out.Vault, err
}

func (c *Client) DeleteEnvVault(ctx context.Context, ws, name string) error {
	return c.do(ctx, http.MethodDelete, envVaultPath(ws, name), nil, nil)
}
