package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

const fileTransferReleasePath = "/v1/file-transfer-sessions/release"

func releaseFileTransferSessions(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Client == nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "CLI authentication is required.")
			return
		}
		var body struct {
			IDs []string `json:"access_session_ids"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(body.IDs) == 0 || len(body.IDs) > 64 {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Provide between 1 and 64 file transfer access session IDs.")
			return
		}
		if err := service.ReleaseFileTransferSessions(r.Context(), p.User.ID, p.Client.SessionID, body.IDs); err != nil {
			writeError(w, r, http.StatusServiceUnavailable, "file_transfer_release_unavailable", "Could not release file transfer credentials. Retry shortly.", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
