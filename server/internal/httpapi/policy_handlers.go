package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// policyAPI is the workspace policy (PLAN F18; contract in shared/policy/README.md).
type policyAPI interface {
	Policy(context.Context, string, string, uint64) (teams.PolicyView, error)
	ValidatePolicy(context.Context, string, string, []byte) (teams.PolicyValidation, error)
	SavePolicy(context.Context, string, string, teams.PolicySaveRequest) (teams.PolicyView, error)
	RestorePolicy(context.Context, string, string, teams.PolicyRestoreRequest) (teams.PolicyView, error)
	ExplainPolicy(context.Context, string, string, teams.PolicyExplainRequest) (teams.PolicyExplanation, error)
	PolicyVersions(context.Context, string, string) ([]teams.PolicyVersion, error)
}

func registerPolicyRoutes(mux *http.ServeMux, service policyAPI, read, write func(http.Handler) http.Handler) {
	mux.HandleFunc("GET /v1/policy/schema", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = w.Write(policy.Schema)
	})
	mux.Handle("GET /v1/workspaces/{ws}/policy", read(policyHandler(service, "get")))
	mux.Handle("GET /v1/workspaces/{ws}/policy/versions", read(policyHandler(service, "versions")))
	mux.Handle("POST /v1/workspaces/{ws}/policy/validate", read(policyHandler(service, "validate")))
	mux.Handle("POST /v1/workspaces/{ws}/policy/explain", read(policyHandler(service, "explain")))
	mux.Handle("PUT /v1/workspaces/{ws}/policy", write(policyHandler(service, "save")))
	mux.Handle("POST /v1/workspaces/{ws}/policy/restore", write(policyHandler(service, "restore")))
}

func policyHandler(service policyAPI, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		account, ws := p.User.ID, r.PathValue("ws")
		if p.APIToken != nil && p.APIToken.Workspace.ID != ws {
			writeError(w, r, http.StatusForbidden, "workspace_unavailable", "This API token belongs to another workspace.")
			return
		}
		var out any
		var err error
		switch kind {
		case "get":
			var version uint64
			if v := r.URL.Query().Get("version"); v != "" {
				if version, err = strconv.ParseUint(v, 10, 64); err != nil || version == 0 {
					writeError(w, r, http.StatusBadRequest, "invalid_request", "version is a positive number.")
					return
				}
			}
			out, err = service.Policy(r.Context(), account, ws, version)
		case "versions":
			var list []teams.PolicyVersion
			list, err = service.PolicyVersions(r.Context(), account, ws)
			out = map[string]any{"versions": list}
		case "validate":
			var in struct {
				Policy string `json:"policy"`
			}
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = service.ValidatePolicy(r.Context(), account, ws, []byte(in.Policy))
		case "explain":
			var in teams.PolicyExplainRequest
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = service.ExplainPolicy(r.Context(), account, ws, in)
		case "save":
			var in teams.PolicySaveRequest
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = service.SavePolicy(r.Context(), account, ws, in)
		case "restore":
			var in teams.PolicyRestoreRequest
			if !decodeStrictJSON(w, r, &in) {
				return
			}
			out, err = service.RestorePolicy(r.Context(), account, ws, in)
		}
		if err != nil {
			writePolicyError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func writePolicyError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *teams.PolicyInvalidError
	var unconfirmed *teams.PolicyUnconfirmedError
	switch {
	case errors.As(err, &invalid):
		writeErrorDetails(w, r, http.StatusUnprocessableEntity, "invalid_policy", "The policy file is not valid; see details.errors.", map[string]any{"errors": invalid.Errors})
	case errors.As(err, &unconfirmed):
		writeErrorDetails(w, r, http.StatusPreconditionFailed, "policy_changes_unconfirmed", "This save changes who can do what. Review details.access_changes and send the request again with confirm.",
			map[string]any{"access_changes": unconfirmed.Changes, "confirm": unconfirmed.Confirm})
	case expectedHTTPErrorOnly(err, teams.ErrPolicyConflict):
		writeError(w, r, http.StatusConflict, "policy_version_conflict", "The policy changed since you read it. Reload it and apply your change again.")
	case expectedHTTPErrorOnly(err, teams.ErrPolicyForbidden):
		writeError(w, r, http.StatusForbidden, "policy_edit_forbidden", "Only owners and admins can change this; members can change access only to the machines they shared.")
	case expectedHTTPErrorOnly(err, teams.ErrPolicyExplainForbidden):
		writeError(w, r, http.StatusForbidden, "policy_explain_forbidden", "Members can explain only their own access; ask an owner or admin.")
	case expectedHTTPErrorOnly(err, teams.ErrPolicyUnavailable):
		writeError(w, r, http.StatusNotFound, "not_found", "That policy version does not exist (only recent versions are kept).")
	case expectedHTTPErrorOnly(err, teams.ErrForbidden), expectedHTTPErrorOnly(err, teams.ErrNotFound):
		writeError(w, r, http.StatusForbidden, "workspace_unavailable", "This workspace is unavailable to the current account.")
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "The workspace policy is temporarily unavailable. Please retry.", err)
	}
}
