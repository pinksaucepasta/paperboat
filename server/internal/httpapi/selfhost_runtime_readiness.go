package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/selfhost"
)

func (h *SelfhostHandlers) RuntimeReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" || r.ContentLength != 0 {
		writeError(w, r, 400, "invalid_request", "Node readiness accepts no resource selector or request body.", nil)
		return
	}
	credential, ok := bearerToken(r)
	if !ok || len(r.Header.Values("Authorization")) != 1 {
		writeError(w, r, 401, "runtime_unauthorized", "Supply this node's runtime credential.", nil)
		return
	}
	out, err := h.Service.RuntimeReadiness(r.Context(), credential, controlplane.ControlTunnelNodeStaleAfter())
	if errors.Is(err, selfhost.ErrUnauthorized) {
		writeError(w, r, 401, "runtime_unauthorized", "This node's credential is no longer accepted; contact its operator.", nil)
		return
	}
	if err != nil {
		writeError(w, r, 503, "runtime_readiness_unavailable", "Node readiness is unavailable; retry shortly.", err)
		return
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Data: out})
}
