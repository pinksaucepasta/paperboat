package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
)

type hostedEdgeLister interface {
	ListHostedEdges(context.Context, string, string, int) (controlplane.HostedEdgePage, error)
}

func hostedEdgeList(service hostedEdgeLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok || principal.User.ID == "" {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Limit must be between 1 and 100.")
				return
			}
			limit = parsed
		}
		page, err := service.ListHostedEdges(r.Context(), principal.User.ID, r.URL.Query().Get("after"), limit)
		if err != nil {
			if errors.Is(err, controlplane.ErrInvalidEdgePage) {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Use a valid edge cursor and limit.")
			} else {
				writeError(w, r, http.StatusServiceUnavailable, "edge_inventory_unavailable", "Hosted edge inventory is unavailable. Retry later.", err)
			}
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: page})
	}
}
