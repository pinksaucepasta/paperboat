package api

import (
	"fmt"
	hostconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ResponseDecodeError preserves the protocol cause without displaying response
// keys, values, paths or JSON decoder text, which may contain private data.
type ResponseDecodeError struct{ Err error }

func (*ResponseDecodeError) Error() string { return "paperboat-server returned an invalid response" }
func (e *ResponseDecodeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func safeRequiredProtocol(value string) string {
	if len(value) == 0 || len(value) > 16 {
		return ""
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != value {
		return ""
	}
	return value
}

// Messages are authored recovery advice, never arbitrary response text. The
// protocol code/status remain available independently for machine decisions.
func apiErrorMessage(code string, status int) string {
	if message, ok := publicAPIMessages[code]; ok {
		return message
	}
	switch status {
	case http.StatusUnauthorized:
		return "The server rejected the credential. Run `pb login` and try again."
	case http.StatusForbidden:
		return "The server denied access to this resource. Check your permissions."
	case http.StatusNotFound:
		return "The requested resource was not found. Check the resource and try again."
	case http.StatusConflict:
		return "The resource state changed. Refresh it and try again."
	case http.StatusRequestTimeout:
		return "The server request timed out. Try again."
	case http.StatusTooManyRequests:
		return "The server rate limit was reached. Wait before trying again."
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "The server rejected the request. Check the supplied values."
	case http.StatusServiceUnavailable:
		return "The service is unavailable. Try again shortly."
	}
	if status >= 500 && status <= 599 {
		return "The server could not complete the request. Try again shortly."
	}
	return "The server rejected the request."
}

// apiErrorMessageFor is apiErrorMessage plus the one server-provided detail a recovery message
// may carry: the plan limit and the console Billing page for the plan-limit refusals.
func apiErrorMessageFor(code string, status int, details map[string]any) string {
	billing := billingURL(details)
	switch code {
	case "bandwidth_quota_exhausted":
		if billing != "" {
			return apiErrorMessage(code, status) + " Add bandwidth: " + billing
		}
	case "machine_limit_reached", "preview_limit_reached":
		return planLimitMessage(code, details, billing)
	case "account_deletion_console_only":
		if console := consoleURL(details); console != "" {
			return "Deleting an account is done in the Paperboat console: " + console
		}
	}
	return apiErrorMessage(code, status)
}

// planLimitMessage names the plan's limit and the two ways out: free one up, or upgrade.
func planLimitMessage(code string, details map[string]any, billing string) string {
	limit, _ := details["limit"].(float64)
	message := apiErrorMessage(code, http.StatusConflict)
	if limit >= 1 && limit == float64(int64(limit)) {
		if code == "machine_limit_reached" {
			message = fmt.Sprintf("This workspace's plan allows %d machines. Remove machines you no longer use (`pb machine revoke <machine>`) or upgrade.", int64(limit))
		} else {
			message = fmt.Sprintf("This workspace's plan allows %d running previews and tunnels. Stop one you no longer need, or upgrade.", int64(limit))
		}
	}
	if billing != "" {
		message = strings.TrimSuffix(message, ".") + ": " + billing
	}
	return message
}

// consoleURL is the console page a console-only refusal names, when it is an http(s) URL.
func consoleURL(details map[string]any) string {
	return httpURL(details["console_url"])
}

// billingURL is the console Billing link a plan-limit refusal may carry, when it is an http(s) URL.
func billingURL(details map[string]any) string {
	return httpURL(details["billing_url"])
}

func httpURL(value any) string {
	raw, _ := value.(string)
	if parsed, err := url.Parse(raw); raw != "" && err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil {
		return parsed.String()
	}
	return ""
}

// PublicMessage is the authored recovery advice for an audited protocol code, or "".
func PublicMessage(code string) string { return publicAPIMessages[code] }

// PublicCode projects only audited protocol codes. The raw Code remains
// available for internal branching and is never made safe by identifier syntax.
func (e *APIError) PublicCode() string {
	if e == nil {
		return "api_error"
	}
	if _, ok := publicAPIMessages[e.Code]; ok {
		return e.Code
	}
	return "api_error"
}

var publicAPIMessages = map[string]string{
	"peer_network_too_large":          "This account has more computers than one peer network configuration can carry, so direct and relayed peer connections are off on this computer; exec, terminals and previews keep working through Paperboat. Remove computers you no longer use (`pb machine list`, then `pb machine revoke`).",
	"peer_network_conflict":           "This computer's peer network key changed while it was registering. Paperboat retries automatically.",
	"update_required":                 "This Paperboat version is no longer supported. Run `pb update` to continue.",
	"access_denied":                   "Access was denied. Sign in with an authorized account.",
	"authorization_pending":           "Authorization is pending. Complete sign-in in the browser.",
	"account_deletion_console_only":   "Deleting an account is done in the Paperboat console. Run `pb account delete` for the link.",
	"machine_limit_reached":           "This workspace's plan machine limit is reached. Remove machines you no longer use (`pb machine revoke <machine>`) or upgrade under Billing in the Paperboat console.",
	"preview_limit_reached":           "This workspace's plan limit for running previews and tunnels is reached. Stop one you no longer need, or upgrade under Billing in the Paperboat console.",
	"bandwidth_quota_exhausted":       "This workspace's hosted relay and edge bandwidth is used up. Direct machine-to-machine access still works. Add bandwidth or upgrade under Billing in the Paperboat console, or wait for the next allowance period.",
	"conflict":                        "The resource state changed. Refresh it and try again.",
	"config_compare_stale":            "The conflict changed before it could be compared. Run `pb config conflict list` and compare the current conflict.",
	"config_compare_host_unavailable": "The machine with the conflict is offline or not connected. Start Paperboat on it, then compare again.",
	"terminal_session_limit_reached":  hostconfig.TerminalCeilingMessage(),
	"credits_exhausted":               "Credits are exhausted. Top up credits before trying again.",
	"entitlement_lost":                "Your Paperboat plan is inactive. Restore billing access before trying again.",
	"expired_token":                   "The authorization token expired. Start sign-in again.",
	"favorite_limit_reached":          "The favorite limit was reached. Remove a favorite before adding another.",
	"forbidden":                       "The server denied access to this resource. Check your permissions.",
	"incompatible_client_version":     "This CLI is incompatible with the server; upgrade pb.",
	"internal":                        "The server could not complete the request. Try again shortly.",
	"invalid_server_response":         "The server returned an invalid response. Try again shortly.",
	"invalid_workspace":               "The selected workspace is unavailable. Run `pb switch personal` or choose another workspace.",
	"machine_capability_unavailable":  "This machine is not configured to host terminals.",
	"machine_not_ready":               "The machine is not ready yet. Wait and try again.",
	"machine_offline":                 "This terminal host is offline. Start Paperboat on the machine and try again.",
	"machine_revoked":                 "Machine access was revoked. Pair the machine again to restore access.",
	"not_found":                       "The requested resource was not found. Check the resource and try again.",
	"payment_required":                "Your Paperboat plan is inactive. Restore billing access before trying again.",
	"rate_limited":                    "The server rate limit was reached. Wait before trying again.",
	"service_unavailable":             "The service is unavailable. Try again shortly.",
	"slow_down":                       "Authorization polling is too frequent. Wait before trying again.",
	"team_subscription_active":        "This team still has an active subscription or a pending checkout. Cancel the subscription or resolve the checkout under Billing in the Paperboat console, then delete the team.",
	"team_subscription_required":      "An active team subscription is required. Check team billing before trying again.",
	"team_suspended":                  "This team is suspended, so it cannot be changed. Ask a team owner to restore billing; access returns when the team resumes.",
	"tunnel_unavailable":              "The secure tunnel is not available yet. Try again shortly.",
	"unauthenticated":                 "The server rejected the credential. Run `pb login` and try again.",
	"workspace_unavailable":           "The selected workspace is unavailable. Run `pb switch personal` or choose another workspace.",
	"policy_version_conflict":         "The policy was changed by someone else since you read it. Run the command again to work from the latest version.",
	"policy_changes_unconfirmed":      "This save changes who has access and was not confirmed. Review the access changes and confirm them.",
	"invalid_policy":                  "The policy file is not valid. Fix the listed errors and try again.",
	"policy_edit_forbidden":           "You cannot save this policy change. Owners and admins edit the policy; members may only change rules for their own machines.",
	"policy_explain_forbidden":        "Only team owners and admins can explain another member's access.",
	"api_token_name_taken":            "You already have an API token with this name in this workspace. Choose another name or revoke the old token.",
	"invalid_api_token_scope":         "The API token scope is not valid. Fix the listed errors and try again.",
	"api_token_invalid":               "The API token expired or was revoked. Create a new one with `pb api-token create`.",
	"outside_token_scope":             "This API token's scope does not cover this. Use a token whose scope includes it, or sign in with `pb login`.",
	"team_sso_required":               "This team requires single sign-on. Run `pb login` and sign in through your company's identity provider, then retry.",
	"directory_managed":               "This member is managed by your company directory. Change their membership or role in your identity provider.",
	"invitation_domain_not_verified":  "This team requires SSO, so invitations go only to its verified domains. Invite an address at a verified domain, or verify the domain with `pb team sso setup <team> --step domain`.",
}

// Operation errors are read projections, not HTTP errors. Keep typed machine
// fields but never treat arbitrary server prose as safe recovery guidance.
// The v1 operation producer owns this single repair action.
func projectOperationError(value *PreviewTunnelAPIError) {
	if value == nil {
		return
	}
	value.Message = "The operation did not complete."
	if message, ok := publicAPIMessages[value.Code]; ok {
		value.Message = message
	}
	if value.RepairAction != "inspect_operation" {
		value.RepairAction = ""
	}
}
