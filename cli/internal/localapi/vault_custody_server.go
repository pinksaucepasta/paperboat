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

// serveVaultCustodyHandoff is the owner-only export (old daemon) and import (new daemon) of a
// daemon replacement. Bodies hold key material and are never logged or echoed in errors.
func (s *Server) serveVaultCustodyHandoff(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "method not allowed")
		return
	}
	if s.config.VaultCustody == nil {
		writeError(w, http.StatusServiceUnavailable, requestID, "vault_custody_unavailable", "ENV memory custody is unavailable")
		return
	}
	var req VaultCustodyHandoff
	defer req.Clear()
	if r.Header.Get("Content-Type") != "application/json" || decodeStrictJSON(http.MaxBytesReader(w, r.Body, vaultCustodyHandoffMaximumJSONBytes), &req) != nil {
		writeError(w, http.StatusBadRequest, requestID, "vault_custody_invalid", "invalid ENV memory custody request")
		return
	}
	var response VaultCustodyHandoff
	var err error
	switch req.Operation {
	case "export":
		response, err = s.config.VaultCustody.Export()
	case "import":
		err = s.config.VaultCustody.Import(req)
	default:
		err = ErrVaultCustodyInvalid
	}
	defer response.Clear()
	if err != nil {
		status, code, message := http.StatusBadRequest, "vault_custody_invalid", "invalid ENV memory custody request"
		switch {
		case errors.Is(err, ErrVaultCustodyStale):
			status, code, message = http.StatusConflict, "vault_custody_stale", "ENV vault memory custody changed; unlock again"
		case errors.Is(err, ErrVaultCustodyLimit):
			status, code, message = http.StatusTooManyRequests, "vault_custody_limit", "ENV vault memory custody limit reached"
		}
		writeError(w, status, requestID, code, message)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	_ = json.NewEncoder(w).Encode(response)
}
