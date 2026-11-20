package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

const operationAccessReleasePath = "/v1/operation-access-sessions/release"

func releaseOperationAccess(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		limitEnvironmentJSON(w, r, 1024)
		var in struct {
			MachineID   string `json:"machine_id"`
			OperationID string `json:"operation_id"`
			Capability  string `json:"capability"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		err := service.ReleaseOperationAccess(r.Context(), p.User.ID, p.Client.SessionID, in.MachineID, in.OperationID, in.Capability)
		switch {
		case errors.Is(err, usermachines.ErrExecOperationInvalid):
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Provide a machine, operation ID and exec or managed_ssh capability.")
		case err != nil:
			writeError(w, r, http.StatusServiceUnavailable, "operation_release_unavailable", "Could not release operation credentials. Retry shortly.", err)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}
