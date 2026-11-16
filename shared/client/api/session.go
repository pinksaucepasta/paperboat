package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

type TokenSet struct {
	AccessToken        string `json:"access_token"`
	RefreshToken       string `json:"refresh_token"`
	TokenType          string `json:"token_type"`
	ExpiresIn          int    `json:"expires_in"`
	Scope              string `json:"scope"`
	CLIClientSessionID string `json:"cli_client_session_id"`
	// Enrollment is the one-shot machine enrollment an approved login for a joining device carries.
	Enrollment *DeviceEnrollment `json:"enrollment,omitempty"`
}

// DeviceEnrollment is the machine enrollment minted for the workspace the account owner chose.
type DeviceEnrollment struct {
	BootstrapToken string    `json:"bootstrap_token"`
	ServerURL      string    `json:"server_url"`
	ExpiresAt      time.Time `json:"expires_at"`
	WorkspaceID    string    `json:"workspace_id"`
	// Admission is "pending" when the team must accept the computer before it joins.
	Admission string `json:"admission"`
}

// Client receives credentials and product identification from its owner.
type Client struct {
	Workspace  string
	BaseURL    string
	Options    CallOptions
	Credential func(context.Context) (string, error)
}

func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	path, err := WorkspaceRequestPath(c.Workspace, path)
	if err != nil {
		return err
	}
	var token string
	if c.Credential != nil {
		token, err = c.Credential(ctx)
		if err != nil {
			return err
		}
	}
	return Call(ctx, c.BaseURL, method, path, body, token, out, c.Options)
}
func (c *Client) RevokeToken(ctx context.Context, token string) error {
	return Call(ctx, c.BaseURL, http.MethodPost, "/v1/auth/token/revoke", nil, token, nil, c.Options)
}
func (c *Client) RefreshToken(ctx context.Context, token, attemptID string) (TokenSet, error) {
	var out TokenSet
	err := Call(ctx, c.BaseURL, http.MethodPost, "/v1/auth/token/refresh", map[string]string{"attempt_id": attemptID}, token, &out, c.Options)
	return out, err
}

// machineID names the paired machine the caller runs on ("" for an SDK endpoint), so the session can act from it.
func (c *Client) StartAPITokenSession(ctx context.Context, token, label, deviceType, osName, machineID string) (TokenSet, error) {
	var out TokenSet
	body := map[string]string{"client_label": label, "device_type": deviceType, "os": osName}
	if machineID != "" {
		body["enrolled_machine_id"] = machineID
	}
	err := Call(ctx, c.BaseURL, http.MethodPost, "/v1/auth/api-token-sessions", body, token, &out, c.Options)
	return out, err
}

// RecipeSession is a CLI session started from a recipe token, with the recipe it pairs the machine to.
type RecipeSession struct {
	TokenSet
	Recipe struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Revision  int64           `json:"revision"`
		Workspace string          `json:"workspace"`
		Recipe    json.RawMessage `json:"recipe"`
	} `json:"recipe"`
}

// StartRecipeSession exchanges a recipe token for a CLI session of the recipe's creator, limited to
// the recipe's machine (shared/recipe/README.md). Only the token id and its derived auth leave this
// computer, never the secret.
func (c *Client) StartRecipeSession(ctx context.Context, token recipe.Token, label, deviceType, osName string) (RecipeSession, error) {
	var out RecipeSession
	body := map[string]any{"token_id": token.ID, "auth": token.Auth(), "client_label": label, "device_type": deviceType, "os": osName}
	err := Call(ctx, c.BaseURL, http.MethodPost, "/v1/auth/recipe-sessions", body, "", &out, c.Options)
	return out, err
}
