//go:build darwin || linux || windows

package runtime

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"net/http"
)

// BindOwnerUpdateGate exposes the existing deployment gate only to the native
// owner, through the already private loopback listener.
func (h *Host) BindOwnerUpdateGate(capability []byte) error {
	if len(capability) != 32 || h.updateGate == nil {
		return ErrHostInvalid
	}
	mux, ok := h.handler.(*http.ServeMux)
	if !ok {
		return ErrHostInvalid
	}
	token := append([]byte(nil), capability...)
	mux.HandleFunc("/v1/local/runtime-update-gate", func(w http.ResponseWriter, r *http.Request) {
		raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Owner-Capability"))
		if r.Method != "POST" || err != nil || subtle.ConstantTimeCompare(raw, token) != 1 {
			http.Error(w, "unavailable", 403)
			return
		}
		var request hostdproto.UpdateGateRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&request) != nil || request.Validate() != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		result, err := h.updateGate.HandleUpdateGate(r.Context(), request)
		if err != nil {
			http.Error(w, "update gate unavailable", 503)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
	return nil
}
