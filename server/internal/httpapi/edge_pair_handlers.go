package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// edgePairService is what the edge pair routes need of the self-host service.
type edgePairService interface {
	EdgePair(ctx context.Context, actor string, scope selfhost.Scope) (selfhost.EdgePair, error)
	SetEdgePair(ctx context.Context, actor string, scope selfhost.Scope, primaryID, fallbackID string) (selfhost.EdgePair, error)
}

// edgePairScope is the workspace of ?workspace= (Personal when absent), as the self-host service names it.
func edgePairScope(r *http.Request, actor string) selfhost.Scope {
	if scope, present, _ := requestWorkspace(r); present && scope.TeamID != "" {
		return selfhost.Scope{Kind: "team", ID: scope.TeamID}
	}
	return selfhost.Scope{Kind: "account", ID: actor}
}

func edgePairGet(service edgePairService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.User.ID == "" {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		pair, err := service.EdgePair(r.Context(), principal.User.ID, edgePairScope(r, principal.User.ID))
		if err != nil {
			writeEdgePairError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, SuccessResponse{Data: pair})
	}
}

func edgePairPut(service edgePairService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.User.ID == "" {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var body struct {
			PrimaryID  string `json:"primary_id"`
			FallbackID string `json:"fallback_id"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		pair, err := service.SetEdgePair(r.Context(), principal.User.ID, edgePairScope(r, principal.User.ID), body.PrimaryID, body.FallbackID)
		if err != nil {
			writeEdgePairError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, SuccessResponse{Data: pair})
	}
}

func writeEdgePairError(w http.ResponseWriter, r *http.Request, err error) {
	var live *selfhost.LivePublicationsError
	switch {
	case errors.As(err, &live):
		writeErrorDetails(w, r, http.StatusConflict, "previews_running", "Stop this workspace's running previews and tunnels before switching between Paperboat's edges and your own servers.", map[string]any{"count": live.Count})
	case expectedHTTPErrorOnly(err, selfhost.ErrUnauthorized):
		writeError(w, r, http.StatusForbidden, "forbidden", "Only the owner or an admin of this workspace can change its edge pair.")
	case expectedHTTPErrorOnly(err, selfhost.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	case expectedHTTPErrorOnly(err, selfhost.ErrEdgePairUnassigned):
		writeError(w, r, http.StatusNotFound, "edge_pair_unassigned", "No edge can serve this workspace yet. Try again in a minute.")
	default:
		writeError(w, r, http.StatusServiceUnavailable, "edge_pair_unavailable", "The edge pair is unavailable. Retry later.", err)
	}
}
