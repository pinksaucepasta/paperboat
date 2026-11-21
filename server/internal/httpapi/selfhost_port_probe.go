package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

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
	sum := sha256.Sum256([]byte(credential))
	if allowed, wait := h.PortProbeLimit.Allow(hex.EncodeToString(sum[:])); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Port probes are sent too often. Retry after the delay.", nil)
		return
	}
	var in struct {
		Transport string          `json:"transport"`
		Port      int             `json:"port"`
		Nonce     json.RawMessage `json:"nonce"`
	}
	var nonce string
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<10))
	decoder := json.NewDecoder(bytes.NewReader(body))
	// One object only, no repeated key (a second "nonce" would override the first after it was
	// checked); nonce absent or a non-empty string.
	if err != nil || !uniqueTopLevelKeys(body) || decoder.Decode(&in) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		in.Nonce != nil && (json.Unmarshal(in.Nonce, &nonce) != nil || nonce == "") {
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

// uniqueTopLevelKeys reports whether body starts with a JSON object whose keys are all distinct.
func uniqueTopLevelKeys(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	return true
}
