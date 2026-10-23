package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/browserterminal"
	"github.com/pinksaucepasta/paperboat-server/internal/peeridentity"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

type browserMachineSignerReader interface {
	MachineTransportSigner(context.Context, string, string, time.Time) (peeridentity.AccountKey, error)
}

func terminalBrowserTicket(gateway *browserterminal.Gateway, signer browserMachineSignerReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Session.ID == "" || p.Client != nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "A browser sign-in is required.")
			return
		}
		var input struct {
			BrowserPublicKeySHA256 string `json:"browser_public_key_sha256"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		ticket, streamURL, decision, err := gateway.Issue(r.Context(), p.User.ID, p.Session.ID, r.PathValue("session_id"), r.Header.Get("Origin"), input.BrowserPublicKeySHA256)
		writeTerminalBrowserTicket(w, r, signer, ticket, streamURL, decision, err)
	}
}
func writeTerminalBrowserTicket(w http.ResponseWriter, r *http.Request, signer browserMachineSignerReader, ticket browserterminal.Ticket, streamURL string, decision browserterminal.Decision, err error) {
	if writeBrowserTerminalQuotaExhausted(w, r, err) {
		return
	}
	switch {
	case expectedHTTPErrorOnly(err, browserterminal.ErrBrowserKeyInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_browser_key", "Create a new browser terminal key and retry.")
	case expectedHTTPErrorOnly(err, browserterminal.ErrForbidden, browserterminal.ErrTicketInvalid):
		writeError(w, r, http.StatusNotFound, "terminal_session_not_found", "Terminal session is unavailable.")
	case err != nil:
		writeError(w, r, http.StatusServiceUnavailable, "terminal_unavailable", "Browser terminal is temporarily unavailable. Retry shortly.", err)
	default:
		if signer == nil {
			writeError(w, r, http.StatusServiceUnavailable, "terminal_unavailable", "Machine identity is temporarily unavailable.")
			return
		}
		machineKey, keyErr := signer.MachineTransportSigner(r.Context(), decision.OwnerAccountID, decision.MachineID, time.Now().UTC())
		if keyErr != nil {
			writeError(w, r, http.StatusServiceUnavailable, "terminal_unavailable", "Machine identity is temporarily unavailable.", keyErr)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"stream_url": streamURL, "subprotocols": []string{browserterminal.Subprotocol, "pb-ticket." + ticket.Value}, "expires_at": ticket.ExpiresAt, "role": ticket.Role, "machine_id": decision.MachineID, "edge_node_id": decision.EdgeNodeID, "owner_account_id": decision.OwnerAccountID, "root_key_id": machineKey.KeyID, "root_public_key": base64.RawURLEncoding.EncodeToString(machineKey.PublicKey), "root_fingerprint": hex.EncodeToString(machineKey.Fingerprint[:])}})
	}
}

// inspectorBrowserTicket admits a console inspector channel (F17) for one of
// the caller's own inspector credentials. Captures never pass through here:
// the ticket only opens the E2E browser channel to the preview's machine.
func inspectorBrowserTicket(gateway *browserterminal.Gateway, signer browserMachineSignerReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok || p.Session.ID == "" || p.Client != nil {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "A browser sign-in is required.")
			return
		}
		var input struct {
			CredentialID           string `json:"credential_id"`
			BrowserPublicKeySHA256 string `json:"browser_public_key_sha256"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		ticket, streamURL, decision, err := gateway.IssueInspector(r.Context(), p.User.ID, p.Session.ID, input.CredentialID, r.Header.Get("Origin"), input.BrowserPublicKeySHA256)
		switch {
		case expectedHTTPErrorOnly(err, browserterminal.ErrBrowserKeyInvalid):
			writeError(w, r, http.StatusBadRequest, "invalid_browser_key", "Create a new browser key and retry.")
		case expectedHTTPErrorOnly(err, browserterminal.ErrForbidden, browserterminal.ErrTicketInvalid):
			writeError(w, r, http.StatusForbidden, "inspector_not_authorized", "You cannot inspect this preview. Ask its owner for inspect access.")
		case expectedHTTPErrorOnly(err, browserterminal.ErrHostUnavailable):
			writeError(w, r, http.StatusConflict, "machine_unavailable", "The preview's machine is offline. Start Paperboat on it and retry.")
		default:
			writeTerminalBrowserTicket(w, r, signer, ticket, streamURL, decision, err)
		}
	}
}

// writeBrowserTerminalQuotaExhausted answers a terminal ticket refused at the hosted allowance
// cap with 429, the code the console's terminal stops reconnecting on.
func writeBrowserTerminalQuotaExhausted(w http.ResponseWriter, r *http.Request, err error) bool {
	var quota *usermachines.BandwidthQuotaError
	if !errors.As(err, &quota) {
		return false
	}
	details := map[string]any{}
	if quota.BillingURL != "" {
		details["billing_url"] = quota.BillingURL
	}
	writeErrorDetails(w, r, http.StatusTooManyRequests, "bandwidth_quota_exhausted", "This workspace's hosted relay and edge bandwidth is used up. Add bandwidth or upgrade, or wait for the next allowance period.", details)
	return true
}
