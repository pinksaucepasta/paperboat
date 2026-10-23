package httpapi

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"io"
	"net/http"
	"time"
)

func userMachinePairingReset(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body usermachines.PairingResetInput
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, r, 400, "pairing_reset_invalid", "Reset details are invalid. Retry the original command.")
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			writeError(w, r, 400, "pairing_reset_invalid", "Reset must contain one request. Retry the original command.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		result, err := service.ResetPairing(ctx, body)
		if err != nil {
			if writePairingRejection(w, r, err) {
				return
			}
			writeError(w, r, 503, "pairing_reset_unavailable", "Reset could not be confirmed. Retry the same command with its saved recovery state.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: result})
	}
}

func writePairingRejection(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case expectedHTTPErrorOnly(err, usermachines.ErrEnrollmentTokenExpired):
		writeError(w, r, http.StatusGone, "enrollment_token_expired", "This install command has expired. Create a new install command.")
	case expectedHTTPErrorOnly(err, usermachines.ErrEnrollmentTokenUsed):
		writeError(w, r, http.StatusConflict, "enrollment_token_used", "This install command has already been used. Resume the existing installation on its original machine.")
	case expectedHTTPErrorOnly(err, usermachines.ErrEnrollmentTokenInvalid):
		writeError(w, r, http.StatusBadRequest, "enrollment_token_invalid", "This install command is invalid or revoked. Create a new install command.")
	case expectedHTTPErrorOnly(err, usermachines.ErrPairingPlatformUnsupported):
		writeError(w, r, http.StatusBadRequest, "pairing_platform_unsupported", "This platform or architecture is unsupported. Use a supported platform with its matching install command.")

	case expectedHTTPErrorOnly(err, usermachines.ErrPairingResetInvalid):
		writeError(w, r, 400, "pairing_reset_invalid", "Reset needs the exact saved machine identities. Retry the original command.")
	case expectedHTTPErrorOnly(err, usermachines.ErrPairingResetDenied):
		writeError(w, r, 403, "pairing_reset_denied", "The install command does not authorize these exact machine identities. Use a fresh command from their original account.")
	case expectedHTTPErrorOnly(err, usermachines.ErrPairingResetConflict):
		writeError(w, r, 409, "pairing_reset_conflict", "Reset is bound to different recovery state. Resume the original saved command.")
	default:
		return false
	}
	return true
}
