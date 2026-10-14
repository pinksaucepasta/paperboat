package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

func configSyncStatus(service *controlplane.ConfigStatusService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		query, ok := readInventoryQuery(w, r, []string{"disabled", "revoked", "consent_required", "pending", "offline", "conflict", "error", "syncing", "healthy", "watching", "restoring", "warning", "sync_uncertain"}, false)
		if !ok {
			return
		}
		status, err := service.AccountInventory(personalWorkspaceContext(r.Context()), principal.User.ID, controlplane.ConfigStatusInventoryOptions{Limit: query.Limit, Offset: query.Offset, Q: query.Q, State: query.State, Owner: query.Owner})
		if err != nil {
			if errors.Is(err, workspace.ErrInvalid) {
				writeWorkspaceError(w, r, err)
				return
			}
			if errors.Is(err, workspace.ErrForbidden) {
				writeWorkspaceError(w, r, err)
				return
			}
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: status})
	})
}

func configSyncEnvironmentStatus(service *controlplane.ConfigStatusService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		status, err := service.AccountInventory(personalWorkspaceContext(r.Context()), principal.User.ID, controlplane.ConfigStatusInventoryOptions{Limit: 1, EnvironmentID: r.PathValue("environment_id")})
		if err != nil {
			if errors.Is(err, workspace.ErrInvalid) || errors.Is(err, workspace.ErrForbidden) {
				writeWorkspaceError(w, r, err)
				return
			}
			writeError(w, r, 500, "internal_error", "Configuration status could not be read.", err)
			return
		}
		if len(status.Environments) != 1 {
			writeError(w, r, 404, "not_found_or_forbidden", "Configuration was not found in this workspace.")
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: status.Environments[0]})
	})
}
