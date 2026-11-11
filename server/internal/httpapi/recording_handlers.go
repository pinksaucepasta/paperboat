package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/recordings"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// Teams session recordings (shared/recording). Team routes use the team route guards; the machine policy is read with
// the machine's runtime credential and proof, like the audit backlog.
func registerRecordingRoutes(mux *http.ServeMux, s *recordings.Service, read, write func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/teams/{team_id}/recordings", read(recordingHandler(s, "list")))
	mux.Handle("GET /v1/teams/{team_id}/recording-key", read(recordingHandler(s, "key")))
	mux.Handle("POST /v1/teams/{team_id}/recording-key", write(recordingHandler(s, "create_key")))
	mux.Handle("PUT /v1/teams/{team_id}/recording-key/grants", write(recordingHandler(s, "grants")))
	mux.Handle("PUT /v1/teams/{team_id}/recording-settings", write(recordingHandler(s, "settings")))
	mux.Handle("POST /v1/recordings/{recording_id}/replay", write(recordingHandler(s, "replay")))
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
		case "replay":
			var in recording.ReplayRequest
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.Replay(ctx, account, r.PathValue("recording_id"), in)
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
