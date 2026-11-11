package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/browserterminal"
	"github.com/pinksaucepasta/paperboat/server/internal/recordings"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// Teams session recordings (shared/recording). Team routes use the team route guards; the machine policy is read with
// the machine's runtime credential and proof, like the audit backlog.
func registerRecordingRoutes(mux *http.ServeMux, s *recordings.Service, gateway *browserterminal.Gateway, read, write func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/teams/{team_id}/recordings", read(recordingHandler(s, "list")))
	mux.Handle("GET /v1/teams/{team_id}/recording-key", read(recordingHandler(s, "key")))
	mux.Handle("POST /v1/teams/{team_id}/recording-key", write(recordingHandler(s, "create_key")))
	mux.Handle("PUT /v1/teams/{team_id}/recording-key/grants", write(recordingHandler(s, "grants")))
	mux.Handle("PUT /v1/teams/{team_id}/recording-settings", write(recordingHandler(s, "settings")))
	mux.Handle("POST /v1/recordings/{recording_id}/replay", write(recordingReplay(s, gateway)))
}

// recordingReplay is POST /v1/recordings/{id}/replay. The service writes the replay audit event first; the browser
// form then gets its single-use browser ticket and stream URL, the CLI form its machine ticket from the service.
func recordingReplay(s *recordings.Service, gateway *browserterminal.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		var in recording.ReplayRequest
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		p, _ := principalFromContext(r.Context())
		browser := in.BrowserPublicKeySHA256 != ""
		if browser && (p.Session.ID == "" || p.Client != nil) {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "A browser sign-in is required to replay in the browser.")
			return
		}
		if browser && gateway == nil {
			writeError(w, r, 503, "recordings_unavailable", "Browser replay is unavailable. Try again.")
			return
		}
		id := r.PathValue("recording_id")
		out, err := s.Replay(r.Context(), account, id, in)
		if err != nil || !browser {
			writeRecordingResult(w, r, out, err)
			return
		}
		ticket, streamURL, _, err := gateway.IssueRecording(r.Context(), account, p.Session.ID, id, r.Header.Get("Origin"), in.BrowserPublicKeySHA256)
		var pending browserterminal.TooManyPendingTicketsError
		switch {
		case err == nil:
			out.Ticket, out.StreamURL = ticket.Value, streamURL
			writeJSON(w, http.StatusOK, SuccessResponse{Data: out})
		case expectedHTTPErrorOnly(err, browserterminal.ErrBrowserKeyInvalid):
			writeError(w, r, http.StatusBadRequest, "invalid_browser_key", "Create a new browser key and retry.")
		case expectedHTTPErrorOnly(err, browserterminal.ErrForbidden, browserterminal.ErrTicketInvalid, browserterminal.ErrSessionInactive):
			writeError(w, r, http.StatusForbidden, recording.ErrorForbidden, "This browser cannot replay the recording. Sign in again and retry.")
		case expectedHTTPErrorOnly(err, browserterminal.ErrHostUnavailable):
			writeError(w, r, http.StatusConflict, "machine_unavailable", "The recording's machine is offline. It can be replayed when the machine is back online.")
		case errors.As(err, &pending):
			writeError(w, r, http.StatusTooManyRequests, "too_many_pending", "Too many replays are opening at once. Wait a moment and retry.")
		default:
			writeError(w, r, 503, "recordings_unavailable", "Browser replay is unavailable. Try again.", err)
		}
	}
}

func recordingHandler(s *recordings.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 2<<20)
		team, ctx := r.PathValue("team_id"), r.Context()
		var out any
		var err error
		switch action {
		case "list":
			f := recordings.Filter{MachineID: r.URL.Query().Get("machine"), SessionID: r.URL.Query().Get("session")}
			if f.Limit, err = queryInt(r, "limit", 0); err == nil {
				f.Offset, err = queryInt(r, "offset", 0)
			}
			if err != nil {
				err = recordings.ErrInvalid
				break
			}
			out, err = s.List(ctx, account, team, f)
		case "key":
			out, err = s.KeyState(ctx, account, team)
		case "create_key":
			var in recording.KeyCreate
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.CreateKey(ctx, account, team, in)
		case "grants":
			var in recording.GrantsPut
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			err = s.PutGrants(ctx, account, team, in)
			out = map[string]bool{"granted": err == nil}
		case "settings":
			var in recording.Settings
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			err = s.PutSettings(ctx, account, team, in)
			out = in
		}
		writeRecordingResult(w, r, out, err)
	}
}

func writeRecordingResult(w http.ResponseWriter, r *http.Request, out any, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, SuccessResponse{Data: out})
	case errors.Is(err, recordings.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "The recording request is invalid.")
	case errors.Is(err, recordings.ErrNotFound):
		writeError(w, r, 404, recording.ErrorNotFound, "The recording or team is unavailable.")
	case errors.Is(err, recordings.ErrForbidden):
		writeError(w, r, 403, recording.ErrorForbidden, "The team's policy does not let you do this. Ask a team admin for a recording_view rule on this machine.")
	case errors.Is(err, recordings.ErrNeedsTeam):
		writeError(w, r, 409, recording.ErrorTeamPlanRequired, "Session recordings are part of the Teams plan.")
	case errors.Is(err, recordings.ErrKeyUnavailable):
		writeError(w, r, 409, recording.ErrorKeyUnavailable, "The team has no recording key yet. A team admin sets it up in Settings.")
	case errors.Is(err, recordings.ErrKeyPending):
		writeError(w, r, 409, recording.ErrorKeyPending, "You have replay permission but no copy of the recording key yet. Ask a team member who can replay to open Paperboat once.")
	case errors.Is(err, recordings.ErrKeyConflict):
		writeError(w, r, 409, recording.ErrorKeyConflict, "The recording key or a viewer's vault changed. Reload and try again.")
	case errors.Is(err, recordings.ErrTooMany):
		writeError(w, r, 429, "too_many_pending", "Too many replays are open at once. Wait a few minutes and retry.")
	case errors.Is(err, recordings.ErrDeleted):
		writeError(w, r, 410, recording.ErrorRecordingDeleted, "The recording was deleted from its machine.")
	case errors.Is(err, teams.ErrSSORequired):
		writeWorkspaceError(w, r, workspace.ErrSSORequired)
	default:
		writeError(w, r, 503, "recordings_unavailable", "Session recordings are unavailable. Try again.", err)
	}
}

// machineRecordingPolicy is GET /v1/machine/recording-policy for the machine named by its runtime credential.
func machineRecordingPolicy(identities machineRequestVerifier, s *recordings.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		proof, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil || len(proof) == 0 || identities == nil {
			writeError(w, r, 401, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		identity, err := identities.VerifyMachineRequest(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), proof, r.Method, r.URL.Path, nil)
		if err != nil {
			writeMachineProofFailure(w, r, err, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		policy, err := s.MachinePolicy(r.Context(), identity.MachineID)
		switch {
		case errors.Is(err, recordings.ErrMachineGone):
			writeError(w, r, 403, "machine_identity_invalid", "This machine identity is no longer current.")
		case err != nil:
			writeError(w, r, 503, "recordings_unavailable", "The recording policy is unavailable; retry.", err)
		default:
			writeJSON(w, http.StatusOK, SuccessResponse{Data: policy})
		}
	}
}
