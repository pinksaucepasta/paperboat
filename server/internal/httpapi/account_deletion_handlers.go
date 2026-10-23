package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/accountdeletion"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

// accountDeletionBrowserOnly refuses every bearer request, CLI session or API token alike, before any
// authentication: account deletion is only for a browser sign-in in the console.
func accountDeletionBrowserOnly(service *accountdeletion.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			writeAccountDeletionConsoleOnly(w, r, service)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeAccountDeletionConsoleOnly(w http.ResponseWriter, r *http.Request, service *accountdeletion.Service) {
	writeErrorDetails(w, r, http.StatusForbidden, "account_deletion_console_only",
		"Account deletion is only available in the web console, signed in with a browser.",
		map[string]any{"console_url": service.ConsoleURL()})
}

// accountDeletionPreview answers GET /v1/account/deletion-preview.
func accountDeletionPreview(service *accountdeletion.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		preview, err := service.Preview(r.Context(), principal.User, principal.Session.SignedInAt)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: preview})
	})
}

// accountDelete answers DELETE /v1/account with {"confirm_email": "..."}.
func accountDelete(sessions *auth.Service, service *accountdeletion.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var input struct {
			ConfirmEmail string `json:"confirm_email"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4097))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body must be {\"confirm_email\": \"<account email>\"}.")
			return
		}
		err := service.Delete(r.Context(), principal.User, principal.Session.SignedInAt, input.ConfirmEmail)
		var owned *accountdeletion.OwnedTeamsError
		switch {
		case err == nil:
			sessions.ClearSessionCookies(w)
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, accountdeletion.ErrReauthenticationRequired):
			writeError(w, r, http.StatusUnauthorized, "reauthentication_required", "Sign in again to delete the account. Deletion needs a sign-in from the last five minutes.")
		case errors.Is(err, accountdeletion.ErrConfirmationMismatch):
			writeError(w, r, http.StatusBadRequest, "confirmation_mismatch", "Type the account email exactly as shown to confirm deletion.")
		case errors.Is(err, accountdeletion.ErrAccountNotActive):
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
		case errors.As(err, &owned):
			writeErrorDetails(w, r, http.StatusConflict, "account_owns_team",
				"Transfer ownership of your teams, or delete them, before deleting the account.",
				map[string]any{"teams": owned.Teams})
		default:
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
		}
	})
}
