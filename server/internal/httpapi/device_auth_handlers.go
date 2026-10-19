package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/realclientip/realclientip-go"
)

func deviceAuthorize(service *auth.DeviceService, requestNetwork func(*http.Request) string) http.HandlerFunc {
	type request struct {
		ClientID    string   `json:"client_id"`
		ClientLabel string   `json:"client_label"`
		DeviceType  string   `json:"device_type"`
		OS          string   `json:"os"`
		Scopes      []string `json:"scopes"`
		// Enroll asks that approving also enroll this device; EnrolledMachineID says it already is a machine.
		Enroll            bool   `json:"enroll"`
		EnrolledMachineID string `json:"enrolled_machine_id"`
		Workspace         string `json:"workspace"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body request
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		out, err := service.Authorize(r.Context(), auth.DeviceAuthorizationInput{ClientID: body.ClientID, ClientLabel: body.ClientLabel, DeviceType: body.DeviceType, OS: body.OS, Scopes: body.Scopes, Network: requestNetwork(r), Enroll: body.Enroll, EnrolledMachineID: body.EnrolledMachineID, Workspace: body.Workspace})
		if writeDeviceError(w, r, err) {
			return
		}
		noStore(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"device_code": out.DeviceCode, "user_code": out.UserCode, "verification_uri": out.VerificationURI, "verification_uri_complete": out.VerificationURIComplete, "expires_in": out.ExpiresIn, "interval": out.Interval}})
	}
}

func deviceToken(service *auth.DeviceService, requestNetwork func(*http.Request) string) http.HandlerFunc {
	type request struct {
		ClientID   string `json:"client_id"`
		DeviceCode string `json:"device_code"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body request
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		out, err := service.Poll(r.Context(), auth.DeviceTokenInput{ClientID: body.ClientID, DeviceCode: body.DeviceCode, Network: requestNetwork(r)})
		if writeDeviceError(w, r, err) {
			return
		}
		writeTokenSet(w, out)
	}
}

func deviceCancel(service *auth.DeviceService, requestNetwork func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ClientID   string `json:"client_id"`
			DeviceCode string `json:"device_code"`
		}
		if !decodeStrictJSON(w, r, &body) {
			return
		}
		if err := service.Cancel(r.Context(), auth.DeviceTokenInput{ClientID: body.ClientID, DeviceCode: body.DeviceCode, Network: requestNetwork(r)}); writeDeviceError(w, r, err) {
			return
		}
		noStore(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

func deviceRequest(service *auth.DeviceService, issuer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		if err := service.RateAccount(r.Context(), p.User.ID); writeDeviceError(w, r, err) {
			return
		}
		out, err := service.Request(r.Context(), r.PathValue("user_code"))
		if writeDeviceError(w, r, err) {
			return
		}
		noStore(w)
		payload := deviceRequestPayload(out, p.User, issuer)
		machine, enrolled, err := service.EnrolledMachine(r.Context(), p.User.ID, out.EnrolledMachineID)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		if err := addDeviceAdmission(r.Context(), service, p.User.ID, out, payload); err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		if enrolled {
			payload["enrolled_machine"] = map[string]any{"id": machine.ID, "alias": machine.Alias, "workspaces": machine.Workspaces}
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: payload})
	}
}
func deviceDecision(service *auth.DeviceService, issuer string, approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		var body struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if r.ContentLength != 0 && !decodeStrictJSON(w, r, &body) {
			return
		}
		out, err := service.Decide(r.Context(), r.PathValue("user_code"), p.User.ID, approve, body.WorkspaceID)
		if writeDeviceError(w, r, err) {
			return
		}
		noStore(w)
		payload := deviceRequestPayload(out, p.User, issuer)
		if err := addDeviceAdmission(r.Context(), service, p.User.ID, out, payload); err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: payload})
	}
}

// addDeviceAdmission tells the approval page whether the chosen workspace accepts the computer at
// once or waits for an owner or admin.
func addDeviceAdmission(ctx context.Context, service *auth.DeviceService, userID string, out auth.DeviceRequest, payload map[string]any) error {
	if !out.Enroll || out.WorkspaceID == "" {
		return nil
	}
	admission, err := service.Admission(ctx, userID, out.WorkspaceID)
	if err != nil {
		return err
	}
	payload["admission"] = admission
	return nil
}

func tokenRefresh(service *auth.DeviceService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var input struct {
			AttemptID string `json:"attempt_id"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		out, err := service.Refresh(r.Context(), token, input.AttemptID)
		if writeDeviceError(w, r, err) {
			return
		}
		writeTokenSet(w, out)
	}
}
func tokenRevoke(service *auth.DeviceService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		if err := service.RevokeToken(r.Context(), token, "logout"); writeDeviceError(w, r, err) {
			return
		}
		noStore(w)
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"status": "revoked"}})
	}
}

func cliClientSessionsList(service *auth.DeviceService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		limit, err := queryInt(r, "limit", 50)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Query parameters are invalid.")
			return
		}
		offset, err := queryInt(r, "offset", 0)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "validation_failed", "Query parameters are invalid.")
			return
		}
		current := ""
		if p.Client != nil {
			current = p.Client.SessionID
		}
		page, err := service.ListClients(r.Context(), p.User.ID, r.URL.Query().Get("state"), current, limit, offset)
		if writeDeviceError(w, r, err) {
			return
		}
		items := make([]map[string]any, 0, len(page.Items))
		for _, i := range page.Items {
			items = append(items, map[string]any{"cli_client_session_id": i.ID, "client_id": i.ClientID, "client_label": i.ClientLabel, "device_type": i.DeviceType, "os": i.OS, "scopes": i.Scopes, "state": i.State, "created_at": i.CreatedAt, "approved_at": i.ApprovedAt, "last_used_at": i.LastUsedAt, "revoked_at": i.RevokedAt, "revocation_reason": i.RevocationReason, "current": i.Current})
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"items": items, "pagination": map[string]any{"limit": page.Limit, "offset": page.Offset, "total": page.Total, "next_offset": page.NextOffset}}})
	}
}
func cliClientSessionDelete(service *auth.DeviceService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFromContext(r.Context())
		if err := service.RevokeClient(r.Context(), p.User.ID, r.PathValue("cli_client_session_id"), "user_revoked"); err != nil {
			writeClientDeleteError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeClientDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "CLI client session was not found.")
		return
	}
	writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
}

func deviceRequestPayload(out auth.DeviceRequest, user auth.User, issuer string) map[string]any {
	return map[string]any{
		"client_label": out.ClientLabel, "device_type": out.DeviceType, "os": out.OS,
		"scopes": out.Scopes, "issued_at": out.IssuedAt, "expires_at": out.ExpiresAt,
		"user_code": out.UserCode, "state": out.State, "issuer": strings.TrimRight(issuer, "/"),
		"enroll": out.Enroll, "requested_workspace": out.RequestedWorkspace, "workspace_id": out.WorkspaceID,
		"account": map[string]any{"id": user.ID, "email": user.PrimaryEmail, "display_name": user.DisplayName},
	}
}
func writeTokenSet(w http.ResponseWriter, out auth.TokenSet) {
	noStore(w)
	data := map[string]any{"access_token": out.AccessToken, "refresh_token": out.RefreshToken, "token_type": out.TokenType, "expires_in": out.ExpiresIn, "scope": out.Scope, "cli_client_session_id": out.CLIClientSessionID}
	if out.Enrollment != nil {
		data["enrollment"] = map[string]any{"bootstrap_token": out.Enrollment.BootstrapToken, "server_url": out.Enrollment.ServerURL, "expires_at": out.Enrollment.ExpiresAt, "workspace_id": out.Enrollment.WorkspaceID, "admission": out.Enrollment.Admission}
	}
	writeJSON(w, http.StatusOK, SuccessResponse{Data: data})
}
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}
func writeDeviceError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var e *auth.DeviceError
	if !errors.As(err, &e) {
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
		} else {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
		}
		return true
	}
	status := http.StatusBadRequest
	message := "Authorization request is invalid."
	details := map[string]any{}
	switch e.Code {
	case "rate_limited":
		status = http.StatusTooManyRequests
		message = "Too many requests."
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	case "slow_down":
		message = "Polling too quickly."
		details["interval"] = e.Interval
	case "authorization_pending":
		message = "Authorization is pending."
	case "access_denied":
		message = "Authorization was denied."
	case "device_request_expired", "device_request_consumed":
		status = http.StatusGone
		message = "Authorization request is no longer available."
	case "workspace_forbidden":
		status = http.StatusForbidden
		message = "You cannot add machines to that workspace."
	case "workspace_required":
		message = "Choose the workspace this device joins."
	case "device_request_not_pending":
		status = http.StatusConflict
		message = "Authorization request is not pending."
	case "device_request_not_found":
		status = http.StatusNotFound
		message = "Authorization request was not found."
	}
	noStore(w)
	writeErrorDetails(w, r, status, e.Code, message, details)
	return true
}
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body must match the documented schema.")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "validation_failed", "Request body must contain one JSON object.")
		return false
	}
	return true
}
func newRequestNetwork(trustedCIDRs []string) func(*http.Request) string {
	trusted, _ := realclientip.AddressesAndRangesToIPNets(trustedCIDRs...)
	forwarded := realclientip.Must(realclientip.NewRightmostTrustedRangeStrategy("X-Forwarded-For", trusted))
	direct := realclientip.RemoteAddrStrategy{}
	return func(r *http.Request) string {
		peer := direct.ClientIP(nil, r.RemoteAddr)
		peerIP := net.ParseIP(peer)
		if peerIP == nil || !ipInNetworks(peerIP, trusted) {
			stripForwardingHeaders(r.Header)
			return peer
		}
		if client := forwarded.ClientIP(r.Header, r.RemoteAddr); client != "" {
			return client
		}
		stripForwardingHeaders(r.Header)
		return peer
	}
}

type requestNetworkContextKey struct{}

func trustedClientNetwork(trustedCIDRs []string, next http.Handler) http.Handler {
	resolve := newRequestNetwork(trustedCIDRs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		network := resolve(r)
		ctx := context.WithValue(r.Context(), requestNetworkContextKey{}, network)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func resolvedRequestNetwork(r *http.Request) string {
	network, _ := r.Context().Value(requestNetworkContextKey{}).(string)
	return network
}

func ipInNetworks(ip net.IP, networks []net.IPNet) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func stripForwardingHeaders(header http.Header) {
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
		header.Del(name)
	}
}
func queryInt(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	return strconv.Atoi(raw)
}
