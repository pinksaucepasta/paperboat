package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Workspace is one server-authorized resource namespace available to the
// authenticated account. The personal workspace is always identified by the
// stable selector "personal"; team workspaces use their team slug.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Role string `json:"role"`
}

// WorkspacePage is the authenticated workspace inventory returned by the
// control plane.
type WorkspacePage struct {
	Items []Workspace `json:"items"`
}

// ListWorkspaces returns the account's current personal and team workspaces.
// Workspace discovery is deliberately unscoped so a revoked active workspace
// cannot prevent the user from finding another one.
func (c *Client) ListWorkspaces(ctx context.Context) (WorkspacePage, error) {
	var page WorkspacePage
	if err := c.doStrict(ctx, http.MethodGet, "/v1/workspaces", nil, &page); err != nil {
		return WorkspacePage{}, err
	}
	if page.Items == nil {
		page.Items = []Workspace{}
	}
	seen := make(map[string]struct{}, len(page.Items))
	personal := false
	for _, item := range page.Items {
		if !validWorkspaceSelector(item.ID) || strings.TrimSpace(item.Name) == "" {
			return WorkspacePage{}, errors.New("paperboat-server returned an invalid workspace")
		}
		if _, ok := seen[item.ID]; ok {
			return WorkspacePage{}, fmt.Errorf("paperboat-server returned duplicate workspace %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		switch item.ID {
		case "personal":
			personal = true
			if item.Kind != "personal" || item.Role != "owner" {
				return WorkspacePage{}, errors.New("paperboat-server returned an invalid Personal workspace")
			}
		default:
			if item.Kind != "team" || (item.Role != "owner" && item.Role != "admin" && item.Role != "member") {
				return WorkspacePage{}, fmt.Errorf("paperboat-server returned an invalid team workspace %q", item.ID)
			}
		}
	}
	if !personal {
		return WorkspacePage{}, errors.New("paperboat-server did not return the Personal workspace")
	}
	return page, nil
}

// SetWorkspace binds user-facing resource requests made by this API client to
// one workspace. It does not alter credentials, machine identity, or daemon
// state. Pass an empty selector only for clients which are intentionally
// unscoped (for example authentication and daemon enrollment clients).
func (c *Client) SetWorkspace(selector string) error {
	if c == nil {
		return errors.New("paperboat API client is nil")
	}
	selector = strings.TrimSpace(selector)
	if selector != "" && !validWorkspaceSelector(selector) {
		return errors.New("workspace must be `personal` or a valid team slug")
	}
	c.workspace = selector
	return nil
}

// Workspace returns the workspace bound to this client, or the empty string
// for intentionally unscoped clients.
func (c *Client) Workspace() string {
	if c == nil {
		return ""
	}
	return c.workspace
}

func validWorkspaceSelector(selector string) bool {
	if selector == "personal" {
		return true
	}
	if len(selector) < 1 || len(selector) > 63 {
		return false
	}
	if !workspaceSlugAlphanumeric(selector[0]) || !workspaceSlugAlphanumeric(selector[len(selector)-1]) {
		return false
	}
	for index := 1; index < len(selector)-1; index++ {
		value := selector[index]
		if !workspaceSlugAlphanumeric(value) && value != '-' {
			return false
		}
	}
	return true
}

func workspaceSlugAlphanumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

// ValidateWorkspaceSelector checks the local syntax accepted for a workspace
// selector. Server membership and current authority are still validated by
// ListWorkspaces and the scoped resource request.
func ValidateWorkspaceSelector(selector string) error {
	if !validWorkspaceSelector(selector) {
		return errors.New("workspace must be `personal` or a valid team slug")
	}
	return nil
}

func (c *Client) workspaceRequestPath(path string) (string, error) {
	if c == nil || c.workspace == "" || !workspaceScopedPath(path) {
		return path, nil
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil {
		return "", fmt.Errorf("invalid Paperboat API path: %w", err)
	}
	values := parsed.Query()
	if current := values.Get("workspace"); current != "" && current != c.workspace {
		return "", errors.New("request workspace does not match the API client workspace")
	}
	values.Set("workspace", c.workspace)
	parsed.RawQuery = values.Encode()
	return parsed.RequestURI(), nil
}

func (c *Client) bindCreateWorkspace(value *string) error {
	if value == nil {
		return errors.New("workspace create request is nil")
	}
	if c == nil || c.workspace == "" {
		if strings.TrimSpace(*value) != "" {
			return errors.New("workspace create request requires a workspace-bound API client")
		}
		return nil
	}
	if strings.TrimSpace(*value) != "" && strings.TrimSpace(*value) != c.workspace {
		return errors.New("create request workspace does not match the API client workspace")
	}
	*value = c.workspace
	return nil
}

func workspaceScopedPath(path string) bool {
	path = strings.SplitN(path, "?", 2)[0]
	for _, prefix := range []string{
		"/v1/machines",
		"/v1/terminal-sessions",
		"/v1/previews",
		"/v1/tunnels",
		"/v1/favorites",
		"/v1/config-repositories",
		"/v1/config-sync",
		"/v1/environment-variables",
		"/v1/environment/hosts",
		"/v1/environment/scopes",
		"/v1/environment/teams",
		"/v1/environment/grants",
		"/v1/environment/users",
		"/v1/team-inbox",
	} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	// Team collection discovery is account-wide. An exact team resource is
	// scoped so explicit team operations cannot escape the selected workspace.
	return strings.HasPrefix(path, "/v1/teams/")
}

func (c *Client) workspaceRequestError(path string, err error) error {
	if err == nil || c == nil || c.workspace == "" || !workspaceScopedPath(path) {
		return err
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Code == "workspace_unavailable" || apiErr.Code == "invalid_workspace") {
		return &WorkspaceAccessError{Workspace: c.workspace, Err: err}
	}
	return err
}

// WorkspaceAccessError adds actionable recovery while preserving the
// structured server error for callers which need its status or stable code.
type WorkspaceAccessError struct {
	Workspace string
	Err       error
}

func (e *WorkspaceAccessError) Error() string {
	if e == nil || e.Err == nil {
		return "selected workspace is unavailable; run `pb switch personal` or choose another workspace"
	}
	return fmt.Sprintf("workspace %q is unavailable or no longer authorized; run `pb switch personal` or choose another workspace: %v", e.Workspace, e.Err)
}

func (e *WorkspaceAccessError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
