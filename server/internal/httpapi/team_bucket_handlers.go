package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/recordings"
	"github.com/pinksaucepasta/paperboat/server/internal/teambucket"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	shared "github.com/pinksaucepasta/paperboat/shared/teambucket"
)

// The team's own bucket (shared/teambucket): owners and admins configure it and choose its uses.
func registerTeamBucketRoutes(mux *http.ServeMux, s *teambucket.Service, read, write func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/teams/{team_id}/bucket", read(teamBucket(s, "get")))
	mux.Handle("PUT /v1/teams/{team_id}/bucket", write(teamBucket(s, "put")))
	mux.Handle("DELETE /v1/teams/{team_id}/bucket", write(teamBucket(s, "delete")))
}

func teamBucket(s *teambucket.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 16<<10)
		team := r.PathValue("team_id")
		var out any
		var err error
		switch action {
		case "get":
			out, err = s.Get(r.Context(), account, team)
		case "put":
			var in shared.Input
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.Put(r.Context(), account, team, in)
		case "delete":
			err = s.Delete(r.Context(), account, team)
			out = map[string]bool{"removed": err == nil}
		}
		var probe *teambucket.ProbeError
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, SuccessResponse{Data: out})
		case errors.As(err, &probe):
			writeError(w, r, http.StatusUnprocessableEntity, "bucket_check_failed", "The bucket was not saved: "+probe.Error()+".")
		case errors.Is(err, shared.ErrInvalid):
			writeError(w, r, http.StatusBadRequest, "invalid_team_bucket", strings.TrimPrefix(err.Error(), shared.ErrInvalid.Error()+": ")+".")
		case errors.Is(err, teambucket.ErrNotFound):
			writeError(w, r, http.StatusNotFound, "bucket_not_configured", "This team has no bucket.")
		case errors.Is(err, teambucket.ErrForbidden):
			writeError(w, r, http.StatusForbidden, "forbidden", teambucket.ErrForbidden.Error())
		case errors.Is(err, teambucket.ErrNeedsTeam):
			writeError(w, r, http.StatusConflict, "team_plan_required", "The team bucket is part of the Team plan.")
		case errors.Is(err, teams.ErrSSORequired):
			writeWorkspaceError(w, r, workspace.ErrSSORequired)
		default:
			writeError(w, r, http.StatusServiceUnavailable, "team_bucket_unavailable", "The team bucket is unavailable. Try again.", err)
		}
	}
}

// registerMachineArchiveRoutes: a machine asks for upload URLs for its closed recording segments and reports how many
// are in the bucket, with its runtime credential and proof like the audit backlog.
func registerMachineArchiveRoutes(mux *http.ServeMux, identities machineRequestVerifier, s *recordings.Service) {
	mux.HandleFunc("POST /v1/machine/recordings/{recording_id}/upload-urls", machineArchive(identities, s, "upload"))
	mux.HandleFunc("POST /v1/machine/recordings/{recording_id}/archived", machineArchive(identities, s, "archived"))
}

func machineArchive(identities machineRequestVerifier, s *recordings.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10+1))
		proof, perr := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil || len(body) > 16<<10 || perr != nil || len(proof) == 0 || identities == nil {
			writeError(w, r, 401, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		identity, err := identities.VerifyMachineRequest(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		id := r.PathValue("recording_id")
		var out any
		switch action {
		case "upload":
			var in shared.UploadRequest
			if !strictBody(body, &in) {
				writeError(w, r, 400, "invalid_request", "The upload request is not valid.")
				return
			}
			out, err = s.UploadURLs(r.Context(), identity.MachineID, id, in)
		case "archived":
			var in shared.ArchivedRequest
			if !strictBody(body, &in) {
				writeError(w, r, 400, "invalid_request", "The archive report is not valid.")
				return
			}
			err = s.Archived(r.Context(), identity.MachineID, id, in.Segments)
			out = map[string]bool{"recorded": err == nil}
		}
		writeMachineArchiveResult(w, r, out, err)
	}
}

func strictBody(body []byte, into any) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	return d.Decode(into) == nil && d.Decode(&struct{}{}) == io.EOF
}

func writeMachineArchiveResult(w http.ResponseWriter, r *http.Request, out any, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, SuccessResponse{Data: out})
	case errors.Is(err, recordings.ErrNoBucket):
		writeError(w, r, http.StatusConflict, "bucket_not_configured", "The team has no bucket for recordings.")
	case errors.Is(err, recordings.ErrNotFound), errors.Is(err, recordings.ErrDeleted):
		writeError(w, r, http.StatusNotFound, "recording_not_found", "This recording is not known for this machine.")
	case errors.Is(err, recordings.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_request", "The request is not valid.")
	default:
		writeError(w, r, http.StatusServiceUnavailable, "recordings_unavailable", "Recording archive is unavailable; retry.", err)
	}
}
