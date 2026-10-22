package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/apitokens"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// apiTokenAuth authenticates "Authorization: Bearer pbat_…" before routing. The request then
// carries a principal acting as the token's creator, pinned to the token's workspace: no
// selector selects it, another selector is 403 workspace_unavailable. Other credentials pass
// through to the route's own authentication.
func apiTokenAuth(tokens *apitokens.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := bearerToken(r)
		if !ok || !strings.HasPrefix(secret, apitokens.SecretPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if tokens == nil {
			writeAPITokenInvalid(w, r)
			return
		}
		token, err := tokens.Authenticate(r.Context(), secret)
		if errors.Is(err, apitokens.ErrInvalid) {
			writeAPITokenInvalid(w, r)
			return
		}
		if err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "authorization_unavailable", "API token authentication is temporarily unavailable. Please retry.", err)
			return
		}
		ctx := r.Context()
		if selection, ok := ctx.Value(workspaceQueryKey{}).(workspaceQuerySelection); ok && selection.present && selection.err == nil && selection.scope != token.Workspace {
			writeWorkspaceError(w, r, workspace.ErrForbidden)
			return
		}
		if len(token.Scope) > 0 && !scopedTokenRoute(r) {
			// Scope narrowing is enforced on the policy and API-token endpoints only so far
			// (PLAN F18-06); everywhere else a scoped token is refused rather than widened.
			writeError(w, r, http.StatusForbidden, "outside_token_scope", "This API token's scope does not cover this request.")
			return
		}
		ctx = workspace.WithContext(ctx, token.Workspace)
		ctx = apitokens.WithPrincipal(ctx, token)
		ctx = audit.WithAPIToken(ctx, token.TokenID)
		ctx = context.WithValue(ctx, authContextKey{}, principal{User: token.User, APIToken: &token})
		bindSupportOwner(ctx, token.User.ID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeAPITokenInvalid(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusUnauthorized, "api_token_invalid", "The API token is unknown, expired or revoked. Create a new one.")
}

// apiTokenPrincipal returns the authenticated principal when the request carries an API token.
func apiTokenPrincipal(r *http.Request) (principal, bool) {
	p, ok := principalFromContext(r.Context())
	return p, ok && p.APIToken != nil
}

type apiTokenJSON struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Workspace  string              `json:"workspace"`
	CreatedBy  string              `json:"created_by"`
	CreatedAt  time.Time           `json:"created_at"`
	ExpiresAt  time.Time           `json:"expires_at"`
	LastUsedAt *time.Time          `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time          `json:"revoked_at,omitempty"`
	Scope      []policy.ScopeEntry `json:"scope,omitempty"`
}

func apiTokenView(t apitokens.Token) apiTokenJSON {
	return apiTokenJSON{ID: t.ID, Name: t.Name, Workspace: t.Workspace.ID, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt,
		ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, RevokedAt: t.RevokedAt, Scope: t.Scope}
}

// apiTokenRequest resolves {ws} and the acting account. A token may only act in its own
// workspace, and may not create tokens: a new token could be wider than its scope.
func apiTokenRequest(w http.ResponseWriter, r *http.Request, create bool) (string, workspace.Scope, bool) {
	noStore(w)
	p, ok := principalFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
		return "", workspace.Scope{}, false
	}
	ws, err := workspace.Parse(r.PathValue("ws"))
	if err != nil {
		writeWorkspaceError(w, r, workspace.ErrInvalid)
		return "", workspace.Scope{}, false
	}
	if p.APIToken != nil && p.APIToken.Workspace != ws {
		writeWorkspaceError(w, r, workspace.ErrForbidden)
		return "", workspace.Scope{}, false
	}
	if p.APIToken != nil && create {
		writeError(w, r, http.StatusForbidden, "forbidden", "API tokens cannot create API tokens. Sign in to create one.")
		return "", workspace.Scope{}, false
	}
	return p.User.ID, ws, true
}

func apiTokenCreate(tokens *apitokens.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := apiTokenRequest(w, r, true)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 64<<10)
		var in struct {
			Name          string          `json:"name"`
			ExpiresInDays *int            `json:"expires_in_days"`
			Scope         json.RawMessage `json:"scope"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		days := 0
		if in.ExpiresInDays != nil {
			if days = *in.ExpiresInDays; days == 0 {
				days = -1 // explicit 0 is out of range, not the default
			}
		}
		token, secret, err := tokens.Create(r.Context(), account, ws, apitokens.CreateInput{Name: in.Name, ExpiresInDays: days, Scope: in.Scope})
		if err != nil {
			writeAPITokenError(w, r, tokens, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"api_token": apiTokenView(token), "secret": secret})
	}
}

func apiTokenList(tokens *apitokens.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := apiTokenRequest(w, r, false)
		if !ok {
			return
		}
		list, err := tokens.List(r.Context(), account, ws)
		if err != nil {
			writeAPITokenError(w, r, tokens, err)
			return
		}
		out := make([]apiTokenJSON, 0, len(list))
		for _, t := range list {
			out = append(out, apiTokenView(t))
		}
		writeJSON(w, http.StatusOK, map[string]any{"api_tokens": out})
	}
}

func apiTokenRevoke(tokens *apitokens.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ws, ok := apiTokenRequest(w, r, false)
		if !ok {
			return
		}
		token, err := tokens.Revoke(r.Context(), account, ws, r.PathValue("id"))
		if err != nil {
			writeAPITokenError(w, r, tokens, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"api_token": apiTokenView(token)})
	}
}

func writeAPITokenError(w http.ResponseWriter, r *http.Request, tokens *apitokens.Service, err error) {
	var scopeErr *apitokens.ScopeError
	switch {
	case errors.As(err, &scopeErr):
		writeErrorDetails(w, r, http.StatusUnprocessableEntity, "invalid_api_token_scope", "The token scope is not valid.", map[string]any{"errors": scopeErr.Errors})
	case expectedHTTPErrorOnly(err, apitokens.ErrInvalidName):
		writeError(w, r, http.StatusBadRequest, "validation_failed", "A token name is 1-63 lowercase letters, digits, dots, underscores or dashes, like a tag name.")
	case expectedHTTPErrorOnly(err, apitokens.ErrInvalidLifetime):
		writeErrorDetails(w, r, http.StatusBadRequest, "validation_failed", "expires_in_days must be from 1 to the maximum.", map[string]any{"max_expires_in_days": tokens.MaxLifetimeDays()})
	case expectedHTTPErrorOnly(err, apitokens.ErrNameTaken):
		writeError(w, r, http.StatusConflict, "api_token_name_taken", "You already have a token with this name in this workspace. Choose another name or revoke it.")
	case expectedHTTPErrorOnly(err, apitokens.ErrLimit):
		writeError(w, r, http.StatusConflict, "api_token_limit", "You have the maximum number of tokens in this workspace. Revoke one first.")
	case expectedHTTPErrorOnly(err, apitokens.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "No such API token in this workspace.")
	case expectedHTTPErrorOnly(err, workspace.ErrInvalid), expectedHTTPErrorOnly(err, workspace.ErrForbidden):
		writeWorkspaceError(w, r, err)
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "API tokens are temporarily unavailable. Please retry.", err)
	}
}

// scopedTokenRoute lists what a scoped API token may call: reading and explaining its
// workspace's policy and the API-token routes.
func scopedTokenRoute(r *http.Request) bool {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/policy/schema":
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/workspaces/") && (strings.HasSuffix(r.URL.Path, "/policy") || strings.HasSuffix(r.URL.Path, "/policy/versions")):
		return true
	case strings.HasPrefix(r.URL.Path, "/v1/workspaces/") && (strings.HasSuffix(r.URL.Path, "/api-tokens") || strings.Contains(r.URL.Path, "/api-tokens/")):
		return true // the handlers decide (a token cannot create tokens; it can revoke itself)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/workspaces/") && (strings.HasSuffix(r.URL.Path, "/policy/explain") || strings.HasSuffix(r.URL.Path, "/policy/validate")):
		return true
	}
	return false
}
