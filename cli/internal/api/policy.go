package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Workspace policy and API token types follow shared/policy/README.md ("HTTP API",
// "API tokens"). {ws} in every path is "personal" or the team slug.

type PolicyError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PolicyNames struct {
	Users    []string `json:"users"`
	Machines []string `json:"machines"`
	Previews []string `json:"previews"`
	Tunnels  []string `json:"tunnels"`
	Tags     []string `json:"tags"`
	Groups   []string `json:"groups"`
}

type Policy struct {
	Workspace           string      `json:"workspace"`
	Version             int64       `json:"version"`
	Policy              string      `json:"policy"`
	Digest              string      `json:"digest"`
	UpdatedAt           time.Time   `json:"updated_at"`
	UpdatedBy           string      `json:"updated_by"`
	UpdatedWithAPIToken string      `json:"updated_with_api_token,omitempty"`
	Message             string      `json:"message"`
	CanEdit             string      `json:"can_edit"`
	Names               PolicyNames `json:"names"`
}

type PolicyChange struct {
	Change  string   `json:"change"`
	Subject string   `json:"subject"`
	Target  string   `json:"target"`
	Action  string   `json:"action"`
	Ports   []any    `json:"ports,omitempty"`
	Rules   []string `json:"rules,omitempty"`
}

type PolicyTestCounts struct {
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type PolicyValidation struct {
	Valid         bool             `json:"valid"`
	Errors        []PolicyError    `json:"errors"`
	Tests         PolicyTestCounts `json:"tests"`
	AccessChanges []PolicyChange   `json:"access_changes"`
	Confirm       string           `json:"confirm"`
}

type PolicyDecision struct {
	Allowed   bool     `json:"allowed"`
	Reason    string   `json:"reason"`
	Rules     []string `json:"rules"`
	Guardrail string   `json:"guardrail,omitempty"`
	// KeyDelivered is false when the team ENV is allowed but the member has no key yet.
	KeyDelivered *bool `json:"key_delivered,omitempty"`
}

type PolicyAccess struct {
	Subject string   `json:"subject"`
	Target  string   `json:"target"`
	Action  string   `json:"action"`
	Ports   []any    `json:"ports,omitempty"`
	Reason  string   `json:"reason"`
	Rules   []string `json:"rules,omitempty"`
}

type PolicyExplanation struct {
	Decision *PolicyDecision `json:"decision,omitempty"`
	WhoCan   []PolicyAccess  `json:"who_can,omitempty"`
	WhatCan  []PolicyAccess  `json:"what_can,omitempty"`
}

type PolicyExplainRequest struct {
	Policy   string `json:"policy,omitempty"`
	Src      string `json:"src,omitempty"`
	Dst      string `json:"dst,omitempty"`
	Action   string `json:"action,omitempty"`
	Port     int    `json:"port,omitempty"`
	APIToken string `json:"api_token,omitempty"`
}

type PolicyVersion struct {
	Version   int64     `json:"version"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"created_at"`
	Author    string    `json:"author"`
	APIToken  string    `json:"api_token,omitempty"`
	Message   string    `json:"message"`
	Gained    int       `json:"gained"`
	Lost      int       `json:"lost"`
	Kind      string    `json:"kind"`
}

// PolicySave is the PUT body; Restore uses the same fields plus the version to restore.
type PolicySave struct {
	Policy          string `json:"policy,omitempty"`
	Version         int64  `json:"version,omitempty"`
	ExpectedVersion int64  `json:"expected_version"`
	Message         string `json:"message,omitempty"`
	Confirm         string `json:"confirm,omitempty"`
}

type APITokenScope struct {
	Dst   []string `json:"dst"`
	Allow []string `json:"allow"`
	Ports []any    `json:"ports,omitempty"`
}

type APIToken struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Workspace  string          `json:"workspace"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	LastUsedAt *time.Time      `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time      `json:"revoked_at,omitempty"`
	Scope      []APITokenScope `json:"scope,omitempty"`
}

type APITokenCreate struct {
	Name          string          `json:"name"`
	ExpiresInDays int             `json:"expires_in_days"`
	Scope         json.RawMessage `json:"scope,omitempty"`
}

type APITokenCreated struct {
	APIToken APIToken `json:"api_token"`
	Secret   string   `json:"secret"`
}

// PolicyErrorDetails is the typed form of a policy error's details: the validation errors
// (422 invalid_policy, invalid_api_token_scope) and the unconfirmed change list (412).
type PolicyErrorDetails struct {
	Errors        []PolicyError  `json:"errors"`
	AccessChanges []PolicyChange `json:"access_changes"`
	Confirm       string         `json:"confirm"`
}

// PolicyDetails decodes err's details when err is an API error with the given code.
func PolicyDetails(err error, code string) (PolicyErrorDetails, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		return PolicyErrorDetails{}, false
	}
	var out PolicyErrorDetails
	encoded, marshalErr := json.Marshal(apiErr.Details)
	if marshalErr != nil || json.Unmarshal(encoded, &out) != nil {
		return PolicyErrorDetails{}, true
	}
	return out, true
}

func workspacePath(ws, rest string) string {
	return "/v1/workspaces/" + url.PathEscape(ws) + rest
}

// GetPolicy reads the saved policy; version 0 means the current one.
func (c *Client) GetPolicy(ctx context.Context, ws string, version int64) (out Policy, err error) {
	path := workspacePath(ws, "/policy")
	if version > 0 {
		path += "?version=" + strconv.FormatInt(version, 10)
	}
	err = c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) ValidatePolicy(ctx context.Context, ws, text string) (out PolicyValidation, err error) {
	err = c.do(ctx, http.MethodPost, workspacePath(ws, "/policy/validate"), map[string]string{"policy": text}, &out)
	return out, err
}

func (c *Client) SavePolicy(ctx context.Context, ws string, in PolicySave) (out Policy, err error) {
	in.Version = 0
	err = c.do(ctx, http.MethodPut, workspacePath(ws, "/policy"), in, &out)
	return out, err
}

func (c *Client) RestorePolicy(ctx context.Context, ws string, in PolicySave) (out Policy, err error) {
	in.Policy = ""
	err = c.do(ctx, http.MethodPost, workspacePath(ws, "/policy/restore"), in, &out)
	return out, err
}

func (c *Client) ExplainPolicy(ctx context.Context, ws string, in PolicyExplainRequest) (out PolicyExplanation, err error) {
	err = c.do(ctx, http.MethodPost, workspacePath(ws, "/policy/explain"), in, &out)
	return out, err
}

func (c *Client) PolicyVersions(ctx context.Context, ws string) ([]PolicyVersion, error) {
	var out struct {
		Versions []PolicyVersion `json:"versions"`
	}
	err := c.do(ctx, http.MethodGet, workspacePath(ws, "/policy/versions"), nil, &out)
	return out.Versions, err
}

func (c *Client) CreateAPIToken(ctx context.Context, ws string, in APITokenCreate) (out APITokenCreated, err error) {
	err = c.do(ctx, http.MethodPost, workspacePath(ws, "/api-tokens"), in, &out)
	return out, err
}

func (c *Client) APITokens(ctx context.Context, ws string) ([]APIToken, error) {
	var out struct {
		APITokens []APIToken `json:"api_tokens"`
	}
	err := c.do(ctx, http.MethodGet, workspacePath(ws, "/api-tokens"), nil, &out)
	return out.APITokens, err
}

func (c *Client) RevokeAPIToken(ctx context.Context, ws, id string) (APIToken, error) {
	var out struct {
		APIToken APIToken `json:"api_token"`
	}
	err := c.do(ctx, http.MethodDelete, workspacePath(ws, "/api-tokens/"+url.PathEscape(id)), nil, &out)
	return out.APIToken, err
}
