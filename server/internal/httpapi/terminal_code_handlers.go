package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/browserterminal"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

var terminalGuestResourceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func terminalGuestCookieName(sessionID string) string { return "__Host-pb-terminal-" + sessionID }
func terminalGuestCookie(r *http.Request) (string, bool) {
	id := r.PathValue("session_id")
	if !terminalGuestResourceID.MatchString(id) {
		return "", false
	}
	cookies := r.CookiesNamed(terminalGuestCookieName(id))
	if len(cookies) != 1 || cookies[0].Value == "" {
		return "", false
	}
	return cookies[0].Value, true
}
func terminalCodeAccessGet(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		value, err := service.TerminalCodeAccess(r.Context(), accountID, r.PathValue("session_id"))
		if err != nil {
			writeTerminalCodeError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, SuccessResponse{Data: value})
	}
}
func terminalCodeAccessPut(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		var input usermachines.TerminalCodeAccessMutation
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		value, err := service.SetTerminalCodeAccess(r.Context(), accountID, r.PathValue("session_id"), input)
		if err != nil {
			writeTerminalCodeError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, SuccessResponse{Data: value})
	}
}
func terminalCodeJoin(service *usermachines.Service, authentication *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if authentication.ValidateBrowserRequest(r) != nil {
			writeError(w, r, http.StatusForbidden, "origin_failed", "Browser origin validation failed.")
			return
		}
		sessionID := r.PathValue("session_id")
		if !terminalGuestResourceID.MatchString(sessionID) {
			writeTerminalCodeError(w, r, usermachines.ErrTerminalCodeInvalid)
			return
		}
		var input struct {
			Code        string `json:"code"`
			DisplayName string `json:"display_name"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		guest, err := service.JoinTerminalWithCode(r.Context(), sessionID, input.Code, input.DisplayName)
		if err != nil {
			writeTerminalCodeError(w, r, err)
			return
		}
		value, err := service.TerminalBrowserSession(r.Context(), "", guest.ID, sessionID)
		if err != nil {
			// No cookie has transferred this newly created grant to the browser.
			// Cleanup must still run when the failed request was cancelled.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
			cleanupErr := service.DiscardTerminalGuest(cleanupCtx, sessionID, guest.ID)
			if cleanupErr != nil {
				supportreport.CaptureFromContext(cleanupCtx, "paperboat-server", "http_terminal", "cleanup", "terminal_cleanup_failed", cleanupErr)
			}
			cancel()
			writeTerminalCodeError(w, r, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: terminalGuestCookieName(sessionID), Value: guest.Token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: guest.ExpiresAt, MaxAge: int(time.Until(guest.ExpiresAt).Seconds())})
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"session": value, "access_method": "code"}})
	}
}
func terminalBrowserSession(service *usermachines.Service, authentication *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		sessionID := r.PathValue("session_id")
		// Account access wins when currently authorized. No guest cookie is ever an account login.
		user, _, authErr := authentication.AuthenticateRequest(r.Context(), r)
		if authErr == nil {
			value, err := service.TerminalBrowserSession(r.Context(), user.ID, "", sessionID)
			if err == nil {
				writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"session": value, "access_method": "account"}})
				return
			}
			if !errors.Is(err, usermachines.ErrTerminalSessionNotFound) {
				writeTerminalCodeError(w, r, err)
				return
			}
		} else if !errors.Is(authErr, auth.ErrUnauthenticated) && !errors.Is(authErr, auth.ErrWorkOSTemporarilyUnavailable) {
			writeTerminalCodeError(w, r, authErr)
			return
		}
		token, ok := terminalGuestCookie(r)
		if ok {
			guest, err := service.AuthenticateTerminalGuest(r.Context(), sessionID, token)
			if err == nil {
				value, err := service.TerminalBrowserSession(r.Context(), "", guest.ID, sessionID)
				if err == nil {
					writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"session": value, "access_method": "code"}})
					return
				}
				writeTerminalCodeError(w, r, err)
				return
			}
			if !errors.Is(err, usermachines.ErrTerminalCodeInvalid) {
				writeTerminalCodeError(w, r, err)
				return
			}
		}
		// Denial discloses neither whether the terminal exists nor its policy/metadata.
		writeError(w, r, http.StatusNotFound, "terminal_access_required", "Sign in with an allowed account or enter a valid join code.")
	}
}
func terminalGuestBrowserTicket(service *usermachines.Service, authentication *auth.Service, gateway *browserterminal.Gateway, signer browserMachineSignerReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if authentication.ValidateBrowserRequest(r) != nil {
			writeError(w, r, http.StatusForbidden, "origin_failed", "Browser origin validation failed.")
			return
		}
		var input struct {
			BrowserPublicKeySHA256 string `json:"browser_public_key_sha256"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		token, ok := terminalGuestCookie(r)
		if !ok {
			writeTerminalCodeError(w, r, usermachines.ErrTerminalCodeInvalid)
			return
		}
		guest, err := service.AuthenticateTerminalGuest(r.Context(), r.PathValue("session_id"), token)
		if err != nil {
			writeTerminalCodeError(w, r, err)
			return
		}
		ticket, streamURL, decision, err := gateway.IssueGuest(r.Context(), guest.ID, guest.SessionID, r.Header.Get("Origin"), input.BrowserPublicKeySHA256)
		writeTerminalBrowserTicket(w, r, signer, ticket, streamURL, decision, err)
	}
}
func writeTerminalCodeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, usermachines.ErrTerminalCodeRateLimited):
		w.Header().Set("Retry-After", "60")
		writeError(w, r, http.StatusTooManyRequests, "terminal_code_rate_limited", "Too many join attempts. Wait a minute and retry.")
	case errors.Is(err, usermachines.ErrTerminalGuestCapacity):
		writeError(w, r, http.StatusConflict, "terminal_guest_capacity", "This terminal has reached its guest limit. Ask the owner to reset code access.")
	case errors.Is(err, usermachines.ErrTerminalCodeInvalid), errors.Is(err, usermachines.ErrTerminalSessionNotFound):
		writeError(w, r, http.StatusNotFound, "terminal_code_unavailable", "This code is invalid, expired, or no longer available. Ask the owner for a current code.")
	case errors.Is(err, usermachines.ErrTerminalBrowserPolicyInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_terminal_code_access", "Choose view-only or interactive access and an expiry of 1, 8, or 24 hours.")
	case errors.Is(err, usermachines.ErrTerminalBrowserInteractionConfirmationRequired):
		writeError(w, r, http.StatusBadRequest, "interactive_confirmation_required", "Confirm that people with the code can run commands in this terminal.")
	case errors.Is(err, usermachines.ErrTerminalBrowserPolicyConflict):
		writeError(w, r, http.StatusConflict, "terminal_code_access_changed", "Code access changed. Refresh before retrying.")
	case errors.Is(err, usermachines.ErrTerminalBrowserSessionClosed):
		writeError(w, r, http.StatusConflict, "terminal_session_closed", "This terminal has closed or is unavailable.")
	default:
		writeError(w, r, http.StatusServiceUnavailable, "terminal_unavailable", "Terminal access is temporarily unavailable. Retry shortly.", err)
	}
}
