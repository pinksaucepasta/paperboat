package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	pbgithub "github.com/pinksaucepasta/paperboat-server/internal/github"
)

func githubStatus(service *pbgithub.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		status, err := service.Status(r.Context(), p.User.ID)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: status})
	}
}

func githubOAuthStart(authService *auth.Service, service *pbgithub.Service) http.HandlerFunc {
	type request struct {
		RedirectURI string `json:"redirect_uri"`
		ReturnTo    string `json:"return_to"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		if body.ReturnTo != "" && !consolePath(body.ReturnTo) {
			writeError(w, r, http.StatusBadRequest, "invalid_return_to", "return_to must be a console path such as /config.")
			return
		}
		state, err := authService.NewOAuthState()
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		url, err := service.OAuthAuthorizeURL(state, body.RedirectURI)
		if errors.Is(err, pbgithub.ErrClientNotConfigured) {
			writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "GitHub OAuth is not configured.", err)
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		authService.SetOAuthStateCookie(w, state)
		setGitHubReturnTo(w, body.ReturnTo)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"state": state, "authorization_url": url}})
	}
}

func githubOAuthCallback(authService *auth.Service, service *pbgithub.Service) http.HandlerFunc {
	type request struct {
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
		State       string `json:"state"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be valid JSON.")
			return
		}
		if err := authService.ValidateOAuthState(r, body.State); err != nil {
			writeError(w, r, http.StatusForbidden, "oauth_state_failed", "OAuth state validation failed.")
			return
		}
		if body.RedirectURI == "" {
			body.RedirectURI = service.DefaultCallbackURL()
		}
		returnTo := gitHubReturnTo(r)
		status, err := service.CompleteOAuth(r.Context(), p.User.ID, body.Code, body.RedirectURI)
		if err != nil {
			writeGitHubOAuthFailure(w, r, err)
			return
		}
		authService.ClearOAuthStateCookie(w)
		setGitHubReturnTo(w, "")
		writeJSON(w, http.StatusOK, SuccessResponse{Data: struct {
			pbgithub.Status
			ReturnTo string `json:"return_to,omitempty"`
		}{status, returnTo}})
	}
}

// githubOAuthBrowserCallback is where GitHub sends the browser after a console-started
// connection. The browser carries no console session on the API origin, so it is sent on to the
// console's callback page, which completes the exchange through POST /v1/github/oauth/callback
// with the user's session and the state cookie set at start. No body is ever shown here.
func githubOAuthBrowserCallback(service *pbgithub.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		query := url.Values{}
		for _, key := range []string{"code", "state", "error"} {
			if value := r.URL.Query().Get(key); value != "" && len(value) <= 512 {
				query.Set(key, value)
			}
		}
		http.Redirect(w, r, service.ConsoleOrigin()+"/github/callback?"+query.Encode(), http.StatusSeeOther)
	}
}

const githubReturnToCookie = "pb_github_return_to"

// consolePath accepts only a same-origin console path: no scheme, host or backslash tricks.
func consolePath(path string) bool {
	return len(path) <= 256 && strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") &&
		!strings.ContainsAny(path, "\\\r\n\t")
}

func setGitHubReturnTo(w http.ResponseWriter, path string) {
	cookie := &http.Cookie{Name: githubReturnToCookie, Value: url.QueryEscape(path), Path: "/",
		MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
	if path == "" {
		cookie.MaxAge = -1
	}
	http.SetCookie(w, cookie)
}

func gitHubReturnTo(r *http.Request) string {
	cookie, err := r.Cookie(githubReturnToCookie)
	if err != nil {
		return ""
	}
	path, err := url.QueryUnescape(cookie.Value)
	if err != nil || !consolePath(path) {
		return ""
	}
	return path
}

// githubDisconnect revokes the user's stored GitHub authorization. The OAuth grant itself stays
// listed in the user's GitHub settings until they remove it there.
func githubDisconnect(service *pbgithub.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		status, err := service.Disconnect(r.Context(), p.User.ID)
		if expectedHTTPErrorOnly(err, pbgithub.ErrConnectionInUse) {
			writeError(w, r, http.StatusConflict, "github_connection_in_use", "Config sync uses this GitHub connection. Turn off config sync first.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: status})
	}
}

func writeGitHubOAuthFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, pbgithub.ErrMissingScopes):
		logHTTPRejection(r, http.StatusForbidden, "github_scope_denied", err)
		writeError(w, r, http.StatusForbidden, "github_scope_denied", "GitHub authorization is missing required scopes.")
	case expectedHTTPErrorOnly(err, pbgithub.ErrIdentityLinkedToAnotherUser):
		logHTTPRejection(r, http.StatusConflict, "github_identity_conflict", err)
		writeError(w, r, http.StatusConflict, "github_identity_conflict", "This GitHub account is already connected to another Paperboat user.")
	case githubOAuthCredentialRejected(err):
		logHTTPRejection(r, http.StatusUnauthorized, "github_oauth_failed", err)
		writeError(w, r, http.StatusUnauthorized, "github_oauth_failed", "GitHub OAuth callback could not be verified.")
	default:
		writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "GitHub OAuth is temporarily unavailable. Please retry the connection.", err)
	}
}

// Only the provider's finite invalid-code/unauthorized statuses preserve the
// existing OAuth rejection response. A joined status plus another failure is
// operational and stays on the captured 503 path.
func githubOAuthCredentialRejected(err error) bool {
	providerErr, ok := err.(pbgithub.APIError)
	return ok && providerErr.OAuthCredentialRejected()
}

func githubRepositories(service *pbgithub.Service) http.HandlerFunc {
	type repositoryResponse struct {
		Owner         string `json:"owner"`
		Name          string `json:"name"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		CloneURL      string `json:"clone_url"`
		HTMLURL       string `json:"html_url"`
		Private       bool   `json:"private"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		query, valid := readCursorInventoryQuery(w, r, nil)
		if !valid {
			return
		}
		if query.Limit > 100 || query.Q != "" || query.Owner != "" || r.URL.Query().Has("offset") {
			writeError(w, r, 400, "invalid_inventory_query", "Use cursor and limit between 1 and 100.")
			return
		}
		repos, next, err := service.RepositoryInventory(r.Context(), p.User.ID, r.URL.Query().Get("cursor"), query.Limit, false)
		switch {
		case errors.Is(err, pbgithub.ErrNotConnected):
			writeError(w, r, http.StatusConflict, "github_required", "Connect GitHub before listing repositories.")
			return
		case errors.Is(err, pbgithub.ErrMissingScopes):
			writeError(w, r, http.StatusForbidden, "github_scope_denied", "GitHub authorization is missing required scopes.")
			return
		case err != nil:
			writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "GitHub repositories could not be loaded.", err)
			return
		}
		out := make([]repositoryResponse, 0, len(repos))
		for _, repo := range repos {
			out = append(out, repositoryResponse{
				Owner:         repo.Owner,
				Name:          repo.Name,
				FullName:      repo.Owner + "/" + repo.Name,
				DefaultBranch: repo.DefaultBranch,
				CloneURL:      repo.CloneURL,
				HTMLURL:       repo.HTMLURL,
				Private:       repo.Private,
			})
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"items": out, "next_cursor": next}})
	}
}

func githubProvisionConfigRepo(service *pbgithub.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		repo, err := service.ProvisionConfigRepo(r.Context(), p.User.ID, r.Header.Get("Idempotency-Key"))
		switch {
		case errors.Is(err, pbgithub.ErrIdempotencyKeyRequired):
			writeError(w, r, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required.")
			return
		case errors.Is(err, pbgithub.ErrNotConnected):
			writeError(w, r, http.StatusConflict, "github_required", "Connect GitHub before provisioning the config repository.")
			return
		case errors.Is(err, pbgithub.ErrMissingScopes):
			writeError(w, r, http.StatusForbidden, "github_scope_denied", "GitHub authorization is missing required scopes.")
			return
		case err != nil:
			writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "GitHub config repository provisioning failed.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: repo})
	}
}
