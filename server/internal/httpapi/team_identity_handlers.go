package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/teamidentity"
)

// Team SSO and directory sync (PLAN F19-02..06). Reads need security.manage or audit.read;
// changes need security.manage.
func registerTeamIdentityRoutes(mux *http.ServeMux, s *teamidentity.Service, read, write func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/teams/{team_id}/identity", read(teamIdentity(s, "status")))
	mux.Handle("POST /v1/teams/{team_id}/identity/portal", write(teamIdentity(s, "portal")))
	mux.Handle("PUT /v1/teams/{team_id}/identity/sso", write(teamIdentity(s, "sso")))
	mux.Handle("PUT /v1/teams/{team_id}/identity/exempt", write(teamIdentity(s, "exempt")))
	mux.Handle("PUT /v1/teams/{team_id}/identity/directory", write(teamIdentity(s, "directory")))
}

func teamIdentity(s *teamidentity.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 64<<10)
		team := r.PathValue("team_id")
		var out any
		var err error
		switch action {
		case "status":
			out, err = s.Status(r.Context(), account, team)
		case "portal":
			var in struct {
				Intent string `json:"intent"`
			}
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			var link string
			link, err = s.PortalLink(r.Context(), account, team, in.Intent)
			out = map[string]string{"url": link}
		case "sso":
			var in teamidentity.SSOSettings
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.UpdateSSO(r.Context(), account, team, in)
		case "exempt":
			var in struct {
				AccountID string `json:"account_id"`
				Exempt    bool   `json:"exempt"`
			}
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.SetExempt(r.Context(), account, team, in.AccountID, in.Exempt)
		case "directory":
			var in teamidentity.DirectorySettings
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.UpdateDirectory(r.Context(), account, team, in)
		}
		var review *teamidentity.ReviewRequired
		switch {
		case err == nil:
			writeJSON(w, 200, SuccessResponse{Data: out})
		case errors.As(err, &review):
			writeErrorDetails(w, r, http.StatusConflict, "sso_review_required", review.Error(), map[string]any{"members": review.Members, "digest": review.Digest})
		case errors.Is(err, teamidentity.ErrNotFound):
			writeError(w, r, 404, "not_found", "Team is unavailable.")
		case errors.Is(err, teamidentity.ErrForbidden):
			writeError(w, r, 403, "forbidden", teamidentity.ErrForbidden.Error())
		case errors.Is(err, teamidentity.ErrInvalid):
			writeError(w, r, 400, "invalid_request", "Check the SSO or directory setting: session length within the allowed range, a known role and group.")
		case errors.Is(err, teamidentity.ErrTooFewSeats):
			writeError(w, r, 409, "sso_needs_seats", "SSO and directory sync are available to teams with enough seats. Add seats in Billing first.")
		case errors.Is(err, teamidentity.ErrNotReady):
			writeError(w, r, 409, "sso_not_ready", teamidentity.ErrNotReady.Error())
		case errors.Is(err, teamidentity.ErrExemptLimit):
			writeError(w, r, 409, "sso_exempt_limit", "This team already has the most SSO-exempt members allowed.")
		case errors.Is(err, teamidentity.ErrNotConfigured):
			writeError(w, r, 503, "sso_unavailable", "SSO is not available on this server.")
		default:
			writeError(w, r, 503, "sso_provider_unavailable", "The SSO provider did not answer. Try again in a minute.", err)
		}
	}
}
