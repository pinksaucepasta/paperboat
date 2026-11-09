package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/server/internal/workspacedomain"
)

// Workspace domains (PLAN F8-02): GET/POST /v1/domains, GET/DELETE /v1/domains/{domain_id},
// POST /v1/domains/{domain_id}/check.
func workspaceDomainHandler(service *workspacedomain.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		ctx := personalWorkspaceContext(r.Context())
		account, id := p.User.ID, r.PathValue("domain_id")
		var (
			view workspacedomain.View
			err  error
		)
		switch action {
		case "list":
			var views []workspacedomain.View
			if views, err = service.List(ctx, account); err == nil {
				writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"items": views}})
				return
			}
		case "add":
			var body struct {
				Hostname string `json:"hostname"`
			}
			decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&body) != nil {
				writeError(w, r, http.StatusBadRequest, "invalid_request", `Send {"hostname": "dev.example.com"}.`)
				return
			}
			if view, err = service.Add(ctx, account, p.User.ID, body.Hostname); err == nil {
				writeJSON(w, http.StatusCreated, SuccessResponse{Data: view})
				return
			}
		case "get":
			view, err = service.Get(ctx, account, id)
		case "check":
			view, err = service.Check(ctx, account, id)
		case "remove":
			if err = service.Remove(ctx, account, id); err == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if err == nil {
			writeJSON(w, http.StatusOK, SuccessResponse{Data: view})
			return
		}
		writeWorkspaceDomainError(w, r, err)
	}
}

func writeWorkspaceDomainError(w http.ResponseWriter, r *http.Request, err error) {
	var inUse *workspacedomain.InUseError
	switch {
	case errors.As(err, &inUse):
		writeErrorDetails(w, r, http.StatusConflict, "domain_in_use", "Tunnels or previews use names under this domain. Remove those hostnames first.", map[string]any{"used_by": inUse.Uses})
	case errors.Is(err, workspacedomain.ErrInvalidHostname):
		writeError(w, r, http.StatusBadRequest, "invalid_domain", "Enter a domain you control, such as example.com or dev.example.com (no wildcard, no https://).")
	case errors.Is(err, workspacedomain.ErrReserved):
		writeError(w, r, http.StatusBadRequest, "domain_reserved", "That domain belongs to Paperboat. Add a domain you control.")
	case errors.Is(err, workspacedomain.ErrConflict):
		writeError(w, r, http.StatusConflict, "domain_conflict", "Another workspace has verified this domain. Remove it there first, or add a subdomain of it.")
	case errors.Is(err, workspacedomain.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "domain_forbidden", "Only team owners and admins add or remove team domains.")
	case errors.Is(err, workspacedomain.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "domain_not_found", "Domain not found in this workspace.")
	case errors.Is(err, workspacedomain.ErrUnavailable):
		writeError(w, r, http.StatusServiceUnavailable, "domains_unavailable", "Custom domains are not configured on this server.")
	case errors.Is(err, workspace.ErrSSORequired), errors.Is(err, workspace.ErrInvalid), errors.Is(err, workspace.ErrForbidden):
		writeWorkspaceError(w, r, err)
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "The domain request failed.", err)
	}
}
