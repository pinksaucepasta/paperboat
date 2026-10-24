package httpapi

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"net/http"
)

func terminalSessionCatalog(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		query, valid := readInventoryQuery(w, r, []string{"running", "closed", "unavailable"}, true, "live", "machine_id")
		if !valid {
			return
		}
		// live=false returns the stored rows without probing each host (shell completion
		// refreshes); authorization is unchanged.
		live := r.URL.Query().Get("live")
		if live != "" && live != "true" && live != "false" {
			writeError(w, r, 400, "invalid_inventory_query", "Use one valid limit (1–200), nonnegative offset, and supported inventory filters.")
			return
		}
		// machine_id: one machine's sessions (the console's machine page), counted and paged on their own.
		machineID, valid := readMachineIDFilter(w, r)
		if !valid {
			return
		}
		items, total, err := service.TerminalSessionInventory(personalWorkspaceContext(r.Context()), account, usermachines.InventoryOptions{Limit: query.Limit, Offset: query.Offset, Q: query.Q, State: query.State, Owner: query.Owner, Closed: query.Closed, MachineID: machineID, WithoutLiveMetadata: live == "false"})
		if err != nil {
			writeError(w, r, 500, "internal_error", "Terminal sessions could not be loaded.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: map[string]any{"sessions": items, "pagination": inventoryPagination(query.Limit, query.Offset, total, len(items))}})
	}
}
func terminalSessionSharing(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		item, err := service.TerminalSessionSharing(r.Context(), account, r.PathValue("session_id"))
		if errors.Is(err, usermachines.ErrTerminalSessionNotFound) {
			writeError(w, r, 404, "terminal_session_not_found", "Terminal session was not found.")
			return
		}
		if err != nil {
			writeError(w, r, 500, "internal_error", "Terminal sharing could not be loaded.", err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: item})
	}
}
func terminalSessionGrant(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		var in struct {
			TeamID             string `json:"team_id"`
			OperationID        string `json:"operation_id"`
			ExpectedGeneration uint64 `json:"expected_generation"`
			Audience           string `json:"audience"`
			AccountID          string `json:"account_id"`
			Role               string `json:"role"`
			Active             bool   `json:"active"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		item, err := service.GrantTerminalSessionSharing(r.Context(), account, in.TeamID, r.PathValue("session_id"), teams.TerminalSessionGrantRequest{OperationID: in.OperationID, ExpectedGeneration: in.ExpectedGeneration, Audience: in.Audience, AccountID: in.AccountID, Role: in.Role, Active: in.Active})
		writeTerminalSharingMutation(w, r, item, err)
	}
}
func terminalSessionEndSharing(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		var in struct {
			OperationID        string `json:"operation_id"`
			ExpectedGeneration uint64 `json:"expected_generation"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		item, err := service.EndTerminalSessionSharing(r.Context(), account, r.PathValue("session_id"), in.OperationID, in.ExpectedGeneration)
		writeTerminalSharingMutation(w, r, item, err)
	}
}
func terminalSessionRemoveParticipant(service *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		account, ok := vaultScopePrincipal(w, r)
		if !ok {
			return
		}
		var in struct {
			OperationID        string `json:"operation_id"`
			ExpectedGeneration uint64 `json:"expected_generation"`
		}
		if !decodeStrictJSON(w, r, &in) {
			return
		}
		item, err := service.RemoveTerminalParticipant(r.Context(), account, r.PathValue("session_id"), r.PathValue("account_id"), in.OperationID, in.ExpectedGeneration)
		writeTerminalSharingMutation(w, r, item, err)
	}
}
func writeTerminalSharingMutation(w http.ResponseWriter, r *http.Request, item usermachines.SharedTerminalSession, err error) {
	switch {
	case errors.Is(err, teams.ErrForbidden):
		writeError(w, r, 403, "forbidden", "Only the terminal session owner can change sharing.")
	case errors.Is(err, teams.ErrConflict):
		writeError(w, r, 409, "team_changed", "Team changed. Refresh before retrying.")
	case errors.Is(err, teams.ErrInvalid):
		writeError(w, r, 400, "invalid_request", "Check the sharing audience, member, role and operation.")
	case errors.Is(err, teams.ErrLimit):
		writeError(w, r, 409, "terminal_session_limit", "Terminal session sharing limit reached. End unused sharing before retrying.")
	case errors.Is(err, usermachines.ErrTerminalSessionEnded):
		writeError(w, r, 409, "terminal_session_closed", "This terminal session has closed, so it cannot be shared. Start or choose a running session.")
	case errors.Is(err, usermachines.ErrTerminalSharingNotApplied):
		writeError(w, r, 409, "terminal_sharing_not_applied", "The session could not be shared; it may have just closed. Check `pb session list` and retry.")
	case errors.Is(err, teams.ErrNotFound), errors.Is(err, usermachines.ErrTerminalSessionNotFound):
		writeError(w, r, 404, "terminal_session_not_found", "Terminal session was not found.")
	case errors.Is(err, usermachines.ErrMachineCapabilityUnavailable):
		writeError(w, r, http.StatusConflict, "terminal_host_unavailable", "The machine running this session did not answer. Check it with `pb status`, then retry the same command.")
	case err != nil:
		writeError(w, r, 500, "internal_error", "Terminal sharing could not be changed. Retry the same operation.", err)
	default:
		writeJSON(w, 200, SuccessResponse{Data: item})
	}
}
