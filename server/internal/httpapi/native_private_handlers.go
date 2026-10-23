package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/nativeprivateaccess"
)

func nativePrivateGrant(service *nativeprivateaccess.Service) http.HandlerFunc {
	type request struct {
		OperationID  string `json:"operation_id"`
		ResourceKind string `json:"resource_kind"`
		ResourceID   string `json:"resource_id"`
		RouteID      string `json:"route_id"`
		Protocol     string `json:"protocol"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body request
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4097))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Native private access request is invalid.")
			return
		}
		if body.OperationID == "" || strings.TrimSpace(body.OperationID) != body.OperationID || body.ResourceKind != "machine_service" || body.ResourceID == "" || body.RouteID == "" || body.Protocol != "tcp" {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Native private access request is invalid.")
			return
		}
		grant, err := service.Issue(r.Context(), nativeprivateaccess.Request{AccountID: p.User.ID, UserID: p.User.ID, CLIClientSessionID: p.Client.SessionID, OperationID: body.OperationID, ResourceKind: body.ResourceKind, ResourceID: body.ResourceID, RouteID: body.RouteID, Protocol: body.Protocol})
		switch {
		case err == nativeprivateaccess.ErrInvalid:
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Native private access request is invalid.")
		case err == machineservices.ErrTeamPrivateAccessOff:
			writeError(w, r, http.StatusForbidden, "team_private_access_off", "The team has not allowed private access to its shared machines. Ask a team owner or admin to turn it on.")
		case err == nativeprivateaccess.ErrServiceNotAnnounced:
			w.Header().Set("Retry-After", "2")
			writeError(w, r, http.StatusConflict, "service_not_announced", "The machine has not announced this port yet. It is retried automatically; check that the service is listening.")
		case err == nativeprivateaccess.ErrDenied:
			writeError(w, r, http.StatusForbidden, "forbidden", "Native private access is not allowed.")
		case err != nil:
			writeError(w, r, http.StatusServiceUnavailable, "native_private_unavailable", "Native private access is temporarily unavailable.", err)
		default:
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, SuccessResponse{Data: grant})
		}
	}
}
