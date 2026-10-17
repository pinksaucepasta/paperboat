package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

func (s *Server) ownsPeer(peer Peer) bool {
	if s.config.OwnerSID != "" {
		return peer.SID == s.config.OwnerSID
	}
	return peer.UID == s.config.OwnerUID
}
func (s *Server) serveVaultCustody(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "method not allowed")
		return
	}
	if s.config.VaultCustody == nil {
		writeError(w, http.StatusServiceUnavailable, requestID, "vault_custody_unavailable", "ENV memory custody is unavailable")
		return
	}
	var req VaultCustodyRequest
	if r.Header.Get("Content-Type") != "application/json" || decodeStrictJSON(http.MaxBytesReader(w, r.Body, vaultCustodyMaximumJSONBytes), &req) != nil {
		clear(req.Value)
		writeError(w, http.StatusBadRequest, requestID, "vault_custody_invalid", "invalid ENV memory custody request")
		return
	}
	defer clear(req.Value)
	response, err := s.config.VaultCustody.Handle(r.Context(), req)
	if err != nil {
		status, code, message := http.StatusBadRequest, "vault_custody_invalid", "invalid ENV memory custody request"
		switch {
		case errors.Is(err, ErrVaultCustodyStale):
			status, code, message = http.StatusConflict, "vault_custody_stale", "ENV vault memory custody changed; unlock again"
		case errors.Is(err, ErrVaultCustodyLimit):
			status, code, message = http.StatusTooManyRequests, "vault_custody_limit", "ENV vault memory custody limit reached"
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			status, code, message = http.StatusRequestTimeout, "deadline_exceeded", "ENV memory custody request canceled"
		}
		writeError(w, status, requestID, code, message)
		return
	}
	defer clear(response.Value)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	_ = json.NewEncoder(w).Encode(response)
}
