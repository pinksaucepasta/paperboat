package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// Recipe types follow shared/recipe/README.md ("API"). Byte fields are standard base64 in JSON,
// which encoding/json does for []byte.

type Recipe struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Workspace   string          `json:"workspace"`
	Revision    int64           `json:"revision"`
	Token       string          `json:"token"`
	Source      string          `json:"source"`
	Format      string          `json:"format"`
	Recipe      json.RawMessage `json:"recipe"`
	CreatedBy   string          `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	LastUsedAt  *time.Time      `json:"last_used_at"`
	RotatedAt   *time.Time      `json:"rotated_at"`
	Machines    int             `json:"machines"`
	MaxMachines int             `json:"max_machines"`
	PublicKey   []byte          `json:"public_key"`
}

type RecipeMachine struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Online          bool            `json:"online"`
	LastSeenAt      *time.Time      `json:"last_seen_at"`
	AppliedRevision int64           `json:"applied_revision"`
	ApplyState      json.RawMessage `json:"apply_state"`
	RemoveAt        *time.Time      `json:"remove_at"`
	PublicKey       []byte          `json:"public_key"`
}

type RecipeImpact struct {
	Machines []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Online bool   `json:"online"`
	} `json:"machines"`
	Vaults []string `json:"vaults"`
}

// RecipeVaultWrap is a vault key wrapped to a recipe's public key.
type RecipeVaultWrap struct {
	Vault   string `json:"vault"`
	Wrapped []byte `json:"wrapped"`
}

type RecipeCreate struct {
	Name       string            `json:"name"`
	Source     string            `json:"source"`
	Format     string            `json:"format"`
	TokenID    string            `json:"token_id"`
	Auth       []byte            `json:"auth"`
	PublicKey  []byte            `json:"public_key"`
	VaultWraps []RecipeVaultWrap `json:"vault_wraps,omitempty"`
}

type RecipeUpdate struct {
	Source     string            `json:"source"`
	Format     string            `json:"format"`
	VaultWraps []RecipeVaultWrap `json:"vault_wraps,omitempty"`
}

type RecipeUpdated struct {
	Recipe         Recipe `json:"recipe"`
	Previous       Recipe `json:"previous"`
	OnlineMachines int    `json:"online_machines"`
}

type RecipeRotate struct {
	Auth        []byte              `json:"auth"`
	PublicKey   []byte              `json:"public_key"`
	VaultWraps  []RecipeVaultWrap   `json:"vault_wraps,omitempty"`
	Confirm     string              `json:"confirm"`
	MachineKeys []RecipeMachineSeal `json:"machine_keys,omitempty"`
}

type RecipeMachineSeal struct {
	MachineID string `json:"machine_id"`
	Sealed    []byte `json:"sealed"`
}

type RecipeRevoked struct {
	Recipe       Recipe       `json:"recipe"`
	Impact       RecipeImpact `json:"impact"`
	RemovalError string       `json:"removal_error"`
}

// RecipeConfirmation is the details of a 409 confirmation_required.
type RecipeConfirmation struct {
	Confirm string       `json:"confirm"`
	Impact  RecipeImpact `json:"impact"`
}

func recipePath(ws, name string) string {
	return workspacePath(ws, "/recipes/"+url.PathEscape(name))
}

// CreateRecipe is safe to retry: the same token id and auth return the same recipe.
func (c *Client) CreateRecipe(ctx context.Context, ws string, in RecipeCreate) (Recipe, error) {
	var out struct {
		Recipe Recipe `json:"recipe"`
	}
	err := c.do(ctx, http.MethodPost, workspacePath(ws, "/recipes"), in, &out)
	return out.Recipe, err
}

func (c *Client) Recipes(ctx context.Context, ws string) ([]Recipe, error) {
	var out struct {
		Recipes []Recipe `json:"recipes"`
	}
	err := c.do(ctx, http.MethodGet, workspacePath(ws, "/recipes"), nil, &out)
	return out.Recipes, err
}

func (c *Client) GetRecipe(ctx context.Context, ws, name string) (Recipe, []RecipeMachine, error) {
	var out struct {
		Recipe   Recipe          `json:"recipe"`
		Machines []RecipeMachine `json:"machines"`
	}
	err := c.do(ctx, http.MethodGet, recipePath(ws, name), nil, &out)
	return out.Recipe, out.Machines, err
}

func (c *Client) UpdateRecipe(ctx context.Context, ws, name string, in RecipeUpdate) (out RecipeUpdated, err error) {
	err = c.do(ctx, http.MethodPut, recipePath(ws, name), in, &out)
	return out, err
}

func (c *Client) RotateRecipe(ctx context.Context, ws, name string, in RecipeRotate) (Recipe, error) {
	var out struct {
		Recipe Recipe `json:"recipe"`
	}
	err := c.do(ctx, http.MethodPost, recipePath(ws, name)+"/rotate", in, &out)
	return out.Recipe, err
}

func (c *Client) RevokeRecipe(ctx context.Context, ws, name, confirm string) (out RecipeRevoked, err error) {
	err = c.do(ctx, http.MethodPost, recipePath(ws, name)+"/revoke", map[string]string{"confirm": confirm}, &out)
	return out, err
}

// RecipeConfirmationDetails decodes a 409 confirmation_required.
func RecipeConfirmationDetails(err error) (RecipeConfirmation, bool) {
	var apiErr *APIError
	var out RecipeConfirmation
	if !errors.As(err, &apiErr) || apiErr.Code != "confirmation_required" {
		return out, false
	}
	encoded, marshalErr := json.Marshal(apiErr.Details)
	if marshalErr != nil || json.Unmarshal(encoded, &out) != nil || out.Confirm == "" {
		return out, false
	}
	return out, true
}

// RecipeMachineState is what a recipe machine applies (GET /v1/recipe-machine).
type RecipeMachineState struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Revision   int64                `json:"revision"`
	Recipe     json.RawMessage      `json:"recipe"`
	Vaults     []RecipeMachineVault `json:"vaults"`
	MachineKey []byte               `json:"machine_key,omitempty"`
}

type RecipeMachineVault struct {
	Name     string `json:"name"`
	KeyEpoch int    `json:"key_epoch"`
	Revision int64  `json:"revision"`
	Envelope []byte `json:"envelope"`
	Wrapped  []byte `json:"wrapped"`
}

// RecipeMachine reads the recipe this machine's session was paired from. A session that is not a
// recipe session is refused (403).
func (c *Client) RecipeMachine(ctx context.Context) (out RecipeMachineState, err error) {
	err = c.do(ctx, http.MethodGet, "/v1/recipe-machine", nil, &out)
	return out, err
}

// ApplyRecipe reports what each section did for a revision, for pb recipe show and the console, with
// the machine's own public key (pb recipe rotate seals the recipe's new key to it).
func (c *Client) ApplyRecipe(ctx context.Context, revision int64, sections map[string]any, machinePublic []byte) error {
	return c.do(ctx, http.MethodPut, "/v1/recipe-machine/apply", map[string]any{"revision": revision, "sections": sections, "public_key": machinePublic}, nil)
}
