package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

func terminalBrowserAccessPolicyGet(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		policy, err := service.TerminalBrowserAccessPolicy(r.Context(), accountID, r.PathValue("session_id"))
		if errors.Is(err, usermachines.ErrTerminalSessionNotFound) {
			writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Terminal access policy could not be loaded.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: policy})
	}
}

func terminalBrowserAccessPolicyPut(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		var mutation usermachines.TerminalBrowserAccessPolicyMutation
		if !decodeStrictJSON(w, r, &mutation) {
			return
		}
		policy, err := service.SetTerminalBrowserAccessPolicy(r.Context(), accountID, r.PathValue("session_id"), mutation)
		switch {
		case errors.Is(err, usermachines.ErrTerminalSessionNotFound):
			writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session was not found.")
		case errors.Is(err, usermachines.ErrTerminalBrowserPolicyInvalid):
			writeError(w, r, http.StatusBadRequest, "invalid_terminal_access_policy", "Choose a supported audience and provide valid email addresses or *@company.com patterns.")
		case errors.Is(err, usermachines.ErrTerminalBrowserInteractionConfirmationRequired):
			writeError(w, r, http.StatusBadRequest, "interactive_confirmation_required", "Confirm that the named email addresses can run commands in this terminal.")
		case errors.Is(err, usermachines.ErrTerminalBrowserPolicyConflict):
			writeError(w, r, http.StatusConflict, "terminal_access_policy_changed", "Terminal access changed. Refresh the policy before retrying.")
		case err != nil:
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Terminal access policy could not be changed.", err)
		default:
			writeJSON(w, http.StatusOK, SuccessResponse{Data: policy})
		}
	}
}
