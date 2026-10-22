package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/auditstream"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

// Audit log streaming to a SIEM (PLAN F19-10). Reads need audit.read; changes audit.stream.
func registerAuditStreamRoutes(mux *http.ServeMux, s *auditstream.Service, read, write func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/teams/{team_id}/audit-streams", read(auditStream(s, "list")))
	mux.Handle("PUT /v1/teams/{team_id}/audit-streams/{kind}", write(auditStream(s, "configure")))
	mux.Handle("DELETE /v1/teams/{team_id}/audit-streams/{kind}", write(auditStream(s, "remove")))
	mux.Handle("POST /v1/teams/{team_id}/audit-streams/{kind}/test", write(auditStream(s, "test")))
}

func auditStream(s *auditstream.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		limitEnvironmentJSON(w, r, 16<<10)
		team, kind := r.PathValue("team_id"), r.PathValue("kind")
		if action != "list" && kind != "https" && kind != "s3" {
			writeError(w, r, 404, "not_found", "Streams are https or s3.")
			return
		}
		var out any
		var err error
		switch action {
		case "list":
			out, err = s.List(r.Context(), account, team)
		case "configure":
			var in auditstream.Input
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = s.Configure(r.Context(), account, team, kind, in)
		case "remove":
			err = s.Remove(r.Context(), account, team, kind)
			out = map[string]bool{"removed": err == nil}
		case "test":
			err = s.Test(r.Context(), account, team, kind)
			out = map[string]bool{"delivered": err == nil}
		}
		var delivery *auditstream.DeliveryError
		switch {
		case err == nil:
			writeJSON(w, 200, SuccessResponse{Data: out})
		case errors.As(err, &delivery):
			writeError(w, r, 502, "audit_stream_delivery_failed", "The test event was not accepted: "+delivery.Error()+".")
		case errors.Is(err, auditstream.ErrInvalid):
			writeError(w, r, 400, "invalid_audit_stream", "Check the destination: an https URL on the public internet; for S3 also region, bucket, access key ID and secret; a custom header needs its value.")
		case errors.Is(err, auditstream.ErrNotFound):
			writeError(w, r, 404, "not_found", "Team or stream is unavailable.")
		case errors.Is(err, auditstream.ErrForbidden):
			writeError(w, r, 403, "forbidden", auditstream.ErrForbidden.Error())
		case errors.Is(err, auditstream.ErrNeedsTeam):
			writeError(w, r, 409, "team_plan_required", "Audit log streaming is part of the Team plan.")
		case errors.Is(err, teams.ErrSSORequired):
			writeWorkspaceError(w, r, workspace.ErrSSORequired)
		default:
			writeError(w, r, 503, "audit_stream_unavailable", "Audit log streaming is unavailable. Try again.", err)
		}
	}
}
