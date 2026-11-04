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
	// ApproveNewMachines is set for a team that holds members' new machines until an owner or admin accepts them.
	ApproveNewMachines bool `json:"approve_new_machines,omitempty"`
}

// WorkspacePage is the authenticated workspace inventory returned by the
// control plane.
type WorkspacePage struct {
	Items []Workspace `json:"items"`
}

// ListWorkspaces returns the account's current personal and team workspaces.
// Workspace discovery is deliberately unscoped so a revoked active workspace
// cannot prevent the user from finding another one.
func ListWorkspaces(ctx context.Context, do func(context.Context, string, string, any, any) error) (WorkspacePage, error) {
	var page WorkspacePage
	if err := do(ctx, http.MethodGet, "/v1/workspaces", nil, &page); err != nil {
		return WorkspacePage{}, err
	}
	if page.Items == nil {
		page.Items = []Workspace{}
	}
	seen := make(map[string]struct{}, len(page.Items))
	personal := false
	for _, item := range page.Items {
		if !ValidWorkspaceSelector(item.ID) || strings.TrimSpace(item.Name) == "" {
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
			if item.Kind != "team" || !validTeamRole(item.Role) {
				return WorkspacePage{}, fmt.Errorf("paperboat-server returned an invalid team workspace %q", item.ID)
			}
		}
	}
	if !personal {
		return WorkspacePage{}, errors.New("paperboat-server did not return the Personal workspace")
	}
	return page, nil
}

// validTeamRole accepts any role name the server may add (billing, auditor and
// later ones); the server alone decides what a role may do.
func validTeamRole(role string) bool {
	if len(role) == 0 || len(role) > 32 {
		return false
	}
	for _, r := range role {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

func ValidWorkspaceSelector(selector string) bool {
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
	if !ValidWorkspaceSelector(selector) {
		return errors.New("workspace must be `personal` or a valid team slug")
	}
	return nil
}

func WorkspaceRequestPath(workspace, path string) (string, error) {
	if workspace == "" || !WorkspaceScopedPath(path) {
		return path, nil
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil {
		return "", fmt.Errorf("invalid Paperboat API path: %w", err)
	}
	values := parsed.Query()
	if current := values.Get("workspace"); current != "" && current != workspace {
		return "", errors.New("request workspace does not match the API client workspace")
	}
	values.Set("workspace", workspace)
	parsed.RawQuery = values.Encode()
	return parsed.RequestURI(), nil
}

func BindCreateWorkspace(workspace string, value *string) error {
	if value == nil {
		return errors.New("workspace create request is nil")
	}
	if workspace == "" {
		if strings.TrimSpace(*value) != "" {
			return errors.New("workspace create request requires a workspace-bound API client")
		}
		return nil
	}
	if strings.TrimSpace(*value) != "" && strings.TrimSpace(*value) != workspace {
		return errors.New("create request workspace does not match the API client workspace")
	}
	*value = workspace
	return nil
}

func WorkspaceScopedPath(path string) bool {
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
		"/v1/environment/layers",
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
