package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/accountprefs"
)

func accountPreferencesGet(service *accountprefs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		prefs, err := service.Get(r.Context(), p.User.ID)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "preferences_unavailable", "Your preferences could not be read. Retry.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: prefs})
	}
}

func accountPreferencesPatch(service *accountprefs.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var update accountprefs.Update
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&update); err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must be a JSON object of known preferences.")
			return
		}
		prefs, err := service.Set(r.Context(), p.User.ID, update)
		switch {
		case errors.Is(err, accountprefs.ErrInvalid):
			writeError(w, r, http.StatusBadRequest, "invalid_preference", "file_transfer_mode must be automatic or unlimited.")
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "preferences_unavailable", "Your preferences could not be saved. Retry.", err)
		default:
			writeJSON(w, http.StatusOK, SuccessResponse{Data: prefs})
		}
	}
}
