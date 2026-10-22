package httpapi

import (
	"context"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/apitokens"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

type workspaceQuerySelection struct {
	present bool
	scope   workspace.Scope
	err     error
}

type workspaceQueryKey struct{}

// workspaceQueryContext parses the optional selector before routing, then
// leaves account and membership validation to the authenticated handler.
func workspaceQueryContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, present := r.URL.Query()["workspace"]
		selection := workspaceQuerySelection{present: present}
		if present {
			if len(values) != 1 {
				selection.err = workspace.ErrInvalid
			} else {
				selection.scope, selection.err = workspace.Parse(values[0])
			}
		}
		ctx := contextWithWorkspaceQuery(r, selection)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func contextWithWorkspaceQuery(r *http.Request, selection workspaceQuerySelection) context.Context {
	ctx := context.WithValue(r.Context(), workspaceQueryKey{}, selection)
	if selection.present && selection.err == nil {
		ctx = workspace.WithContext(ctx, selection.scope)
	}
	return ctx
}

// BindWorkspaceSelector combines the router's optional query selector with a
// selector inside a signed JSON body. When requireBody is true, a query
// selector must also appear in the signed body; an absent query and body keeps
// the existing resource-bound behavior. Returned requests carry the parsed
// scope in context for downstream services.
func BindWorkspaceSelector(r *http.Request, bodyWorkspace *string, requireBody bool) (*http.Request, workspace.Scope, bool, error) {
	selection, ok := r.Context().Value(workspaceQueryKey{}).(workspaceQuerySelection)
	if !ok {
		values, present := r.URL.Query()["workspace"]
		selection = workspaceQuerySelection{present: present}
		if present {
			if len(values) != 1 {
				selection.err = workspace.ErrInvalid
			} else {
				selection.scope, selection.err = workspace.Parse(values[0])
			}
		}
	}
	if selection.err != nil {
		return r, workspace.Scope{}, false, selection.err
	}
	if requireBody && selection.present && bodyWorkspace == nil {
		return r, workspace.Scope{}, false, workspace.ErrInvalid
	}
	if bodyWorkspace == nil {
		if !selection.present {
			return r, workspace.Scope{}, false, nil
		}
		return r.WithContext(workspace.WithContext(r.Context(), selection.scope)), selection.scope, true, nil
	}
	bodyScope, err := workspace.Parse(*bodyWorkspace)
	if err != nil {
		return r, workspace.Scope{}, false, err
	}
	if selection.present && selection.scope != bodyScope {
		return r, workspace.Scope{}, false, workspace.ErrInvalid
	}
	return r.WithContext(workspace.WithContext(r.Context(), bodyScope)), bodyScope, true, nil
}

func requestWorkspace(r *http.Request) (workspace.Scope, bool, error) {
	_, scope, present, err := BindWorkspaceSelector(r, nil, false)
	return scope, present, err
}

// requireWorkspaceTarget prevents a selected workspace from being used to
// manage an explicitly targeted resource owned by another workspace. Requests
// without a selector keep their existing resource-bound behavior.
func requireWorkspaceTarget(w http.ResponseWriter, r *http.Request, target workspace.Scope) bool {
	selected, present, err := requestWorkspace(r)
	if err != nil {
		writeWorkspaceError(w, r, err)
		return false
	}
	if present && selected != target {
		writeError(w, r, http.StatusConflict, "workspace_target_conflict", "The requested resource does not belong to the selected workspace.")
		return false
	}
	return true
}

func requirePersonalWorkspaceTarget(w http.ResponseWriter, r *http.Request) bool {
	return requireWorkspaceTarget(w, r, workspace.Scope{ID: "personal"})
}

func requireTeamWorkspaceTarget(w http.ResponseWriter, r *http.Request, teamID string) bool {
	return requireWorkspaceTarget(w, r, workspace.Scope{ID: teamID, TeamID: teamID})
}

func personalWorkspaceContext(ctx context.Context) context.Context {
	if _, selected := workspace.FromContext(ctx); selected {
		return ctx
	}
	return workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
}

// ValidateWorkspaceSelection verifies a parsed request scope after the caller
// has established the account or signed machine principal. It is shared by
// ordinary user routes and signed publisher handlers.
func ValidateWorkspaceSelection(ctx context.Context, service *workspace.Service, account string) (workspace.Scope, bool, error) {
	scope, present, err := validateWorkspaceSelection(ctx, service, account)
	if token, ok := apitokens.FromContext(ctx); ok && err == nil && (!present || scope != token.Workspace) {
		// An API token acts only in its own workspace (apiTokenAuth selects it).
		return workspace.Scope{}, false, workspace.ErrForbidden
	}
	return scope, present, err
}

func validateWorkspaceSelection(ctx context.Context, service *workspace.Service, account string) (workspace.Scope, bool, error) {
	if selection, ok := ctx.Value(workspaceQueryKey{}).(workspaceQuerySelection); ok {
		if selection.err != nil {
			return workspace.Scope{}, false, workspace.ErrInvalid
		}
		if selection.present {
			if service == nil {
				return workspace.Scope{}, false, workspace.ErrForbidden
			}
			if err := service.Validate(ctx, account, selection.scope); err != nil {
				return workspace.Scope{}, false, err
			}
			return selection.scope, true, nil
		}
	}
	scope, present := workspace.FromContext(ctx)
	if !present {
		return workspace.Scope{}, false, nil
	}
	if service == nil {
		return workspace.Scope{}, false, workspace.ErrForbidden
	}
	if err := service.Validate(ctx, account, scope); err != nil {
		return workspace.Scope{}, false, err
	}
	return scope, true, nil
}

func requireWorkspaceSelection(service *workspace.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if _, _, err := ValidateWorkspaceSelection(r.Context(), service, p.User.ID); err != nil {
			writeWorkspaceError(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeWorkspaceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, workspace.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_workspace", "Choose Personal or one canonical team workspace.")
	case expectedHTTPErrorOnly(err, workspace.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "workspace_unavailable", "This workspace is unavailable to the current account.")
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Workspace authority could not be verified.", err)
	}
}
