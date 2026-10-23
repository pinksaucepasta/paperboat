package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
)

func writeRouteIntentError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusServiceUnavailable, "control_unavailable", "Route intent could not be completed. Keep the support reference and try again later."
	switch {
	case errors.Is(err, controlplane.ErrRouteInvalid):
		status, code, message = http.StatusBadRequest, "validation_failed", "Route intent is invalid."
	case errors.Is(err, controlplane.ErrRouteConflict):
		status, code, message = http.StatusConflict, "version_conflict", "Route intent changed. Refresh it and retry."
	case errors.Is(err, controlplane.ErrRouteDenied):
		status, code, message = http.StatusNotFound, "not_found_or_forbidden", "Route intent could not be found."
	}
	if status < http.StatusInternalServerError {
		logHTTPRejection(r, status, code, err)
		writeError(w, r, status, code, message)
		return
	}
	writeError(w, r, status, code, message, err)
}

func routeIntentCreate(service *controlplane.RouteService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		var in struct {
			Kind       string `json:"kind"`
			PublicHost string `json:"public_host"`
			TargetHost string `json:"target_host"`
			TargetPort int32  `json:"target_port"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		operationKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if operationKey == "" {
			writeError(w, r, 400, "idempotency_key_required", "Idempotency-Key header is required.")
			return
		}
		item, err := service.Create(r.Context(), p.User.ID, operationKey, r.PathValue("environment_id"), in.Kind, in.PublicHost, in.TargetHost, in.TargetPort)
		if err != nil {
			writeRouteIntentError(w, r, err)
			return
		}
		writeJSON(w, 201, SuccessResponse{Data: item})
	}
}

func routeIntentTransition(service *controlplane.RouteService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		var in struct {
			DesiredState     string `json:"desired_state"`
			ExpectedRevision int64  `json:"expected_revision"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		operationKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if operationKey == "" {
			writeError(w, r, 400, "idempotency_key_required", "Idempotency-Key header is required.")
			return
		}
		item, err := service.Transition(r.Context(), p.User.ID, operationKey, r.PathValue("route_id"), in.DesiredState, in.ExpectedRevision)
		if err != nil {
			writeRouteIntentError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: item})
	}
}
