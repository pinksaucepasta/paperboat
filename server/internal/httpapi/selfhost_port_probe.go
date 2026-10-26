package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// PortProbe lets pbh doctor learn whether this node's public TCP range is reachable from outside:
// the control plane connects to the credential's own node and expects the nonce echoed back.
func (h *SelfhostHandlers) PortProbe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	credential, ok := bearerToken(r)
	if !ok || len(r.Header.Values("Authorization")) != 1 {
		writeError(w, r, 401, "runtime_unauthorized", "Supply this node's runtime credential.", nil)
		return
	}
	var in struct {
		Port  int    `json:"port"`
		Nonce string `json:"nonce"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil {
		writeError(w, r, 400, "invalid_request", "Send {\"port\": <public TCP port>, \"nonce\": <16-64 URL-safe characters>}.", nil)
		return
	}
	out, err := h.Service.ProbePort(r.Context(), credential, in.Port, in.Nonce)
	switch {
	case errors.Is(err, selfhost.ErrPortProbeInvalid):
		writeError(w, r, 400, "invalid_request", "The port must be a public TCP tunnel port and the nonce 16-64 URL-safe characters.", nil)
	case errors.Is(err, selfhost.ErrUnauthorized):
		writeError(w, r, 401, "runtime_unauthorized", "This node's credential is no longer accepted; contact its operator.", nil)
	case err != nil:
		writeError(w, r, 503, "port_probe_unavailable", "The port probe is unavailable; retry shortly.", err)
	default:
		writeJSON(w, http.StatusOK, SuccessResponse{Data: out})
	}
}
