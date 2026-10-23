package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	pbgithub "github.com/pinksaucepasta/paperboat-server/internal/github"
)

func githubNativeStart(service *pbgithub.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, 401, "unauthenticated", "Native client authentication is required.")
			return
		}
		item, err := service.StartNativeLink(r.Context(), p.User.ID, p.Client.SessionID)
		if err != nil {
			if expectedHTTPErrorOnly(err, pbgithub.ErrNativeLink) {
				logHTTPRejection(r, http.StatusForbidden, "forbidden", err)
				writeError(w, r, http.StatusForbidden, "forbidden", "Reconnect this Paperboat machine session before starting a GitHub connection.")
				return
			}
			writeError(w, r, 503, "github_connection_unavailable", "GitHub connection could not be started.", err)
			return
		}
		noStore(w)
		writeJSON(w, 201, SuccessResponse{Data: item})
	}
}
func githubNativeStatus(service *pbgithub.Service, cancel bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, 401, "unauthenticated", "Native client authentication is required.")
			return
		}
		item, err := service.NativeLinkStatus(r.Context(), p.User.ID, p.Client.SessionID, r.PathValue("link_id"), cancel)
		if err != nil {
			if expectedHTTPErrorOnly(err, pbgithub.ErrNativeLink) {
				logHTTPRejection(r, 404, "not_found_or_forbidden", err)
				writeError(w, r, 404, "not_found_or_forbidden", "GitHub connection request is unavailable.")
				return
			}
			writeError(w, r, 503, "github_connection_unavailable", "GitHub connection status is temporarily unavailable.", err)
			return
		}
		noStore(w)
		writeJSON(w, 200, SuccessResponse{Data: item})
	}
}

// githubNativeDecision serves the console's review, approve and cancel of a CLI's GitHub
// connection request. The signed-in user must be the request's account and hold its token
// (the console reads it from the link's fragment). Review changes nothing.
func githubNativeDecision(service *pbgithub.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to the account that started this request.")
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Token == "" {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must carry the connection request's token.")
			return
		}
		id := r.PathValue("link_id")
		var data any
		var err error
		switch action {
		case "review":
			data, err = service.ReviewNativeLink(r.Context(), p.User.ID, id, body.Token)
		case "approve":
			var authorizationURL string
			authorizationURL, err = service.ApproveNativeLink(r.Context(), p.User.ID, id, body.Token)
			data = map[string]string{"authorization_url": authorizationURL}
		default:
			err = service.CancelNativeLink(r.Context(), p.User.ID, id, body.Token)
			data = map[string]string{"state": "canceled"}
		}
		if err != nil {
			writeNativeLinkFailure(w, r, err)
			return
		}
		noStore(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: data})
	}
}

func writeNativeLinkFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case expectedHTTPErrorOnly(err, pbgithub.ErrNativeLinkOtherAccount):
		writeError(w, r, http.StatusForbidden, "github_link_other_account", "This GitHub connection request was started for another Paperboat account. Sign in to that account.")
	case expectedHTTPErrorOnly(err, pbgithub.ErrNativeLinkExpired):
		writeError(w, r, http.StatusGone, "github_link_expired", "This GitHub connection request has expired. Start it again from the Paperboat CLI.")
	case expectedHTTPErrorOnly(err, pbgithub.ErrNativeLinkUsed):
		writeError(w, r, http.StatusConflict, "github_link_used", "This GitHub connection request was already approved or cancelled.")
	case expectedHTTPErrorOnly(err, pbgithub.ErrNativeLink):
		writeError(w, r, http.StatusNotFound, "github_link_unavailable", "This GitHub connection request is not available. Start it again from the Paperboat CLI.")
	case expectedHTTPErrorOnly(err, pbgithub.ErrClientNotConfigured):
		writeError(w, r, http.StatusServiceUnavailable, "provider_unavailable", "GitHub OAuth is not configured.", err)
	default:
		writeError(w, r, http.StatusServiceUnavailable, "github_connection_unavailable", "GitHub connection is temporarily unavailable.", err)
	}
}

// githubCallbackDispatch completes a CLI's request (state pbnative_*) or a console-started
// connection, and sends the browser to the console's result page; the API answers no HTML.
func githubCallbackDispatch(service *pbgithub.Service, browser http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		if !strings.HasPrefix(state, "pbnative_") {
			browser.ServeHTTP(w, r)
			return
		}
		noStore(w)
		if err := service.CompleteNativeLink(r.Context(), state, r.URL.Query().Get("code")); err != nil {
			code := githubCallbackErrorCode(err)
			logHTTPRejection(r, http.StatusSeeOther, code, err)
			http.Redirect(w, r, githubResultURL(service, "", url.Values{"result": {"error"}, "code": {code}}), http.StatusSeeOther)
			return
		}
		login, _ := service.NativeLinkLogin(r.Context(), state)
		http.Redirect(w, r, githubResultURL(service, "", url.Values{"result": {"connected"}, "login": {login}}), http.StatusSeeOther)
	}
}

func githubCallbackErrorCode(err error) string {
	switch {
	case expectedHTTPErrorOnly(err, pbgithub.ErrMissingScopes):
		return "github_scope_denied"
	case expectedHTTPErrorOnly(err, pbgithub.ErrIdentityLinkedToAnotherUser):
		return "github_identity_conflict"
	case expectedHTTPErrorOnly(err, pbgithub.ErrNativeLink):
		return "github_link_expired"
	default:
		return "github_oauth_failed"
	}
}

// githubResultURL is the console's GitHub result page; returnTo is a console path chosen when
// the connection started (validated as a same-origin relative path).
func githubResultURL(service *pbgithub.Service, returnTo string, query url.Values) string {
	if returnTo != "" {
		query.Set("return_to", returnTo)
	}
	return service.ConsoleOrigin() + "/github/done?" + query.Encode()
}
