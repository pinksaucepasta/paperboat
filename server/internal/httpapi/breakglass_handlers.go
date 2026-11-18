package httpapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/server/internal/breakglass"
	"github.com/pinksaucepasta/paperboat/server/internal/operatorauth"
)

// BreakGlassAPI is the guarded recovery run (internal/breakglass).
type BreakGlassAPI interface {
	Preview(ctx context.Context, keepSessionHash []byte) (breakglass.Preview, error)
	Start(ctx context.Context, in breakglass.StartInput) (breakglass.Run, error)
	Get(ctx context.Context, id string) (breakglass.Run, error)
	Cancel(ctx context.Context, id, operatorID string) error
}

// sessionHash is the stored form of the caller's operator session, the one a run keeps signed in.
func sessionHash(r *http.Request) []byte {
	token := operatorCookie(r)
	if token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func breakGlassPreview(api BreakGlassAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		preview, err := api.Preview(r.Context(), sessionHash(r))
		if breakGlassError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: preview})
	}
}

func breakGlassStart(api BreakGlassAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		var body struct {
			Phrase       string `json:"phrase"`
			Reason       string `json:"reason"`
			TOTPCode     string `json:"totp_code"`
			FloorVersion string `json:"floor_version"`
		}
		if !ok || !decodeStrictJSON(w, r, &body) {
			return
		}
		run, err := api.Start(r.Context(), breakglass.StartInput{OperatorID: p.User.ID, KeepSessionHash: sessionHash(r), Phrase: body.Phrase, Reason: body.Reason, TOTPCode: body.TOTPCode, FloorVersion: body.FloorVersion})
		if breakGlassError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusAccepted, SuccessResponse{Data: run})
	}
}

func breakGlassGet(api BreakGlassAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, err := api.Get(r.Context(), r.PathValue("run_id"))
		if breakGlassError(w, r, err) {
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: run})
	}
}

func breakGlassCancel(api BreakGlassAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			return
		}
		if breakGlassError(w, r, api.Cancel(r.Context(), r.PathValue("run_id"), p.User.ID)) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// breakGlassError writes the response for err and reports whether it did.
func breakGlassError(w http.ResponseWriter, r *http.Request, err error) bool {
	var notReady *breakglass.NotReadyError
	switch {
	case err == nil:
		return false
	case errors.Is(err, breakglass.ErrPhrase), errors.Is(err, breakglass.ErrReason), errors.Is(err, breakglass.ErrFloor):
		writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error())
	case errors.Is(err, operatorauth.ErrMFAInvalid):
		writeError(w, r, http.StatusForbidden, "mfa_invalid", "The authenticator code is wrong, expired or already used. Wait for the next code and try again.")
	case errors.As(err, &notReady):
		writeError(w, r, http.StatusConflict, "breakglass_not_ready", err.Error())
	case errors.Is(err, breakglass.ErrRunning), errors.Is(err, breakglass.ErrNotRunning):
		writeError(w, r, http.StatusConflict, "breakglass_conflict", err.Error())
	case errors.Is(err, breakglass.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
	default:
		writeError(w, r, http.StatusServiceUnavailable, "breakglass_unavailable", "Break-glass is unavailable. Try again.", err)
	}
	return true
}
