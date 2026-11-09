package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
)

type hostedEdgeLister interface {
	ListHostedEdges(context.Context, string, string, int) (controlplane.HostedEdgePage, error)
}

type hostedRelayLister interface {
	ListHostedRelays(context.Context, string, string, int) (controlplane.HostedRelayPage, error)
}

func hostedEdgeList(service hostedEdgeLister) http.HandlerFunc {
	return inventoryPageHandler("Hosted edge inventory", "edge_inventory_unavailable", func(ctx context.Context, account, after string, limit int) (any, error) {
		return service.ListHostedEdges(ctx, account, after, limit)
	})
}

func hostedRelayList(service hostedRelayLister) http.HandlerFunc {
	return inventoryPageHandler("Relay inventory", "relay_inventory_unavailable", func(ctx context.Context, account, after string, limit int) (any, error) {
		return service.ListHostedRelays(ctx, account, after, limit)
	})
}

// inventoryPageHandler serves one cursor-paged node inventory (edges, relays) for the signed-in account.
func inventoryPageHandler(what, unavailable string, list func(context.Context, string, string, int) (any, error)) http.HandlerFunc {
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
		page, err := list(r.Context(), principal.User.ID, r.URL.Query().Get("after"), limit)
		if err != nil {
			if errors.Is(err, controlplane.ErrInvalidEdgePage) {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Use a valid cursor and limit.")
			} else {
				writeError(w, r, http.StatusServiceUnavailable, unavailable, what+" is unavailable. Retry later.", err)
			}
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: page})
	}
}
