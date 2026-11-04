package api

import (
	"context"
	"errors"
	"fmt"
	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
	"strings"
)

func (c *Client) ListWorkspaces(ctx context.Context) (clientapi.WorkspacePage, error) {
	return clientapi.ListWorkspaces(ctx, c.doChecked)
}
func ValidateWorkspaceSelector(selector string) error {
	return clientapi.ValidateWorkspaceSelector(selector)
}
func (c *Client) workspaceRequestPath(path string) (string, error) {
	return clientapi.WorkspaceRequestPath(c.Workspace(), path)
}
func (c *Client) bindCreateWorkspace(value *string) error {
	return clientapi.BindCreateWorkspace(c.Workspace(), value)
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
	if selector != "" && !clientapi.ValidWorkspaceSelector(selector) {
		return errors.New("workspace must be `personal` or a valid team slug")
	}
	c.workspace = selector
	return nil
}

// ForWorkspace is this client, with the same credentials and transport, bound to another workspace.
func (c *Client) ForWorkspace(selector string) (*Client, error) {
	if c == nil {
		return nil, errors.New("paperboat API client is nil")
	}
	next := *c
	if err := next.SetWorkspace(selector); err != nil {
		return nil, err
	}
	return &next, nil
}

// Workspace returns the workspace bound to this client, or the empty string
// for intentionally unscoped clients.
func (c *Client) Workspace() string {
	if c == nil {
		return ""
	}
	return c.workspace
}

func (c *Client) workspaceRequestError(path string, err error) error {
	if err == nil || c == nil || c.workspace == "" || !clientapi.WorkspaceScopedPath(path) {
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
