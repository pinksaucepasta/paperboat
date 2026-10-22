package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Team business features (PLAN F19): the audit log and team SSO / directory sync.

// ActivityEvent is one audit log event (server/docs/teams-business.md).
type ActivityEvent struct {
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	Workspace  string    `json:"workspace"`
	Action     string    `json:"action"`
	Actor      struct {
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		Email   string `json:"email,omitempty"`
		TokenID string `json:"token_id,omitempty"`
	} `json:"actor"`
	Target struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Name string `json:"name,omitempty"`
	} `json:"target"`
	Result    string         `json:"result"`
	Reason    string         `json:"reason,omitempty"`
	IP        string         `json:"ip,omitempty"`
	UserAgent string         `json:"user_agent,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details"`
}

type ActivityPage struct {
	Items      []ActivityEvent `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// Activity searches a workspace's audit log; filters are the server's query parameters.
func (c *Client) Activity(ctx context.Context, workspace string, filters url.Values) (ActivityPage, error) {
	var out ActivityPage
	query := cloneValues(filters)
	query.Set("workspace", workspace)
	err := c.do(ctx, http.MethodGet, "/v1/activity?"+query.Encode(), nil, &out)
	return out, err
}

// ExportActivity streams a team's audit log export (format csv or json) into w.
func (c *Client) ExportActivity(ctx context.Context, workspace, format string, filters url.Values, w io.Writer) error {
	query := cloneValues(filters)
	query.Set("workspace", workspace)
	query.Set("format", format)
	path := "/v1/activity/export?" + query.Encode()
	req, _, reference, path, err := c.newRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var envelope apiEnvelope
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope)
		return c.responseError(resp, path, reference, envelope, decodeErr)
	}
	if _, err = io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("download the audit log export: %w", err)
	}
	return nil
}

func cloneValues(in url.Values) url.Values {
	out := url.Values{}
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// TeamIdentity is a team's SSO and directory sync state.
type TeamIdentity struct {
	Eligible     bool `json:"eligible"`
	MinSeats     int  `json:"min_seats"`
	Seats        int  `json:"seats"`
	Organization bool `json:"organization"`
	CanManage    bool `json:"can_manage"`
	SSO          struct {
		Connections []struct {
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"connections"`
		Domains []struct {
			Domain string `json:"domain"`
			State  string `json:"state"`
		} `json:"domains"`
		Required       bool     `json:"required"`
		SessionSeconds int      `json:"session_seconds"`
		JIT            bool     `json:"jit"`
		Exempt         []string `json:"exempt"`
	} `json:"sso"`
	Directory struct {
		Directory *struct {
			Type  string `json:"type"`
			State string `json:"state"`
		} `json:"directory"`
		Users  int `json:"users"`
		Groups []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"groups"`
		MappedOnly bool       `json:"mapped_only"`
		SyncedAt   *time.Time `json:"synced_at,omitempty"`
		Error      string     `json:"error,omitempty"`
	} `json:"directory"`
}

func teamIdentityPath(team, suffix string) string {
	return "/v1/teams/" + url.PathEscape(team) + "/identity" + suffix
}

func (c *Client) TeamIdentity(ctx context.Context, team string) (TeamIdentity, error) {
	var out TeamIdentity
	err := c.do(ctx, http.MethodGet, teamIdentityPath(team, ""), nil, &out)
	return out, err
}

// TeamIdentityPortal returns a short-lived WorkOS Admin Portal link (intent: domain_verification, sso, dsync).
func (c *Client) TeamIdentityPortal(ctx context.Context, team, intent string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	err := c.do(ctx, http.MethodPost, teamIdentityPath(team, "/portal"), map[string]string{"intent": intent}, &out)
	return out.URL, err
}

// TeamSSOSettings changes Require SSO (confirm = review digest), session length or JIT.
type TeamSSOSettings struct {
	Required       *bool  `json:"required,omitempty"`
	SessionSeconds *int   `json:"session_seconds,omitempty"`
	JIT            *bool  `json:"jit,omitempty"`
	Confirm        string `json:"confirm,omitempty"`
}

func (c *Client) UpdateTeamSSO(ctx context.Context, team string, in TeamSSOSettings) (TeamIdentity, error) {
	var out TeamIdentity
	err := c.do(ctx, http.MethodPut, teamIdentityPath(team, "/sso"), in, &out)
	return out, err
}

func (c *Client) SetTeamSSOExempt(ctx context.Context, team, account string, exempt bool) (TeamIdentity, error) {
	var out TeamIdentity
	err := c.do(ctx, http.MethodPut, teamIdentityPath(team, "/exempt"), map[string]any{"account_id": account, "exempt": exempt}, &out)
	return out, err
}

// TeamDirectorySettings maps directory groups to roles ("" removes) and sets mapped-only.
type TeamDirectorySettings struct {
	MappedOnly *bool             `json:"mapped_only,omitempty"`
	GroupRoles map[string]string `json:"group_roles,omitempty"`
}

func (c *Client) UpdateTeamDirectory(ctx context.Context, team string, in TeamDirectorySettings) (TeamIdentity, error) {
	var out TeamIdentity
	err := c.do(ctx, http.MethodPut, teamIdentityPath(team, "/directory"), in, &out)
	return out, err
}
