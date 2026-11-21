package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// PortProbe lets pbh doctor learn whether this node's public TCP range and its own edge and relay ports
// are reachable from outside: the control plane connects to the credential's own node.
func (h *SelfhostHandlers) PortProbe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	credential, ok := bearerToken(r)
	if !ok || len(r.Header.Values("Authorization")) != 1 {
		writeError(w, r, 401, "runtime_unauthorized", "Supply this node's runtime credential.", nil)
		return
	}
	var in struct {
		Transport string          `json:"transport"`
		Port      int             `json:"port"`
		Nonce     json.RawMessage `json:"nonce"`
	}
	var nonce string
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	// One object only; nonce absent or a string, never null.
	if decoder.Decode(&in) != nil || decoder.Decode(&struct{}{}) != io.EOF || in.Nonce != nil && (string(in.Nonce) == "null" || json.Unmarshal(in.Nonce, &nonce) != nil) {
		writeError(w, r, 400, "invalid_request", "Send {\"transport\": \"tcp\"|\"udp\", \"port\": <port>, \"nonce\": <16-64 URL-safe characters, public TCP ports only>}.", nil)
		return
	}
	out, err := h.Service.ProbePort(r.Context(), credential, in.Transport, in.Port, nonce)
	switch {
	case errors.Is(err, selfhost.ErrPortProbeInvalid):
		writeError(w, r, 400, "invalid_request", "Probe a public TCP tunnel port with a 16-64 URL-safe character nonce, a TCP edge or relay node port without one, or the relay's STUN port on UDP.", nil)
	case errors.Is(err, selfhost.ErrUnauthorized):
		writeError(w, r, 401, "runtime_unauthorized", "This node's credential is no longer accepted; contact its operator.", nil)
	case err != nil:
		writeError(w, r, 503, "port_probe_unavailable", "The port probe is unavailable; retry shortly.", err)
	default:
		writeJSON(w, http.StatusOK, SuccessResponse{Data: out})
	}
}
