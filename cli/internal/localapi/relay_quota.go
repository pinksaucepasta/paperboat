package localapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// RelayQuotaCode is the local API code for a peer connection that failed because the workspace's
// hosted relay allowance is used up.
const RelayQuotaCode = "bandwidth_quota_exhausted"

// TokenScopeCode is the local API code for a peer connection the control plane refused because
// the CLI session's API token scope does not cover the target (F18-06).
const TokenScopeCode = "outside_token_scope"

// NoEligiblePathCode is the local API code for an Unlimited file transfer stream that was not opened (or was
// closed) because the only path to the machine is a Paperboat-hosted relay or there is none. The sender waits
// and resumes when a direct connection or the account's own relay returns.
const NoEligiblePathCode = "no_eligible_path"

// NoEligiblePathError is the daemon's refusal of an Unlimited stream; Path is the path kind it found.
type NoEligiblePathError struct{ Path string }

func (e *NoEligiblePathError) Error() string {
	if e.Path == "hosted_relay" {
		return "Waiting for a direct connection or your relay: only a Paperboat relay reaches this machine now"
	}
	return "Waiting for a direct connection or your relay: this machine is not reachable now"
}

// Refusal reports whether a local API code is a refusal whose message is shown as it is: the
// connection was refused before anything was sent or started, and retrying does not help.
// AuthenticationRequiredCode answers a request the daemon cannot serve because this machine is signed out.
const AuthenticationRequiredCode = "authentication_required"

func Refusal(code string) bool { return code == RelayQuotaCode || code == TokenScopeCode }

// writeServerRefusal answers a failed peer operation with the refusal its error carries (the
// relay plan limit, or an API token scope that does not cover the target), so the caller can name
// it and the action instead of a generic connectivity failure. It reports whether it answered.
func writeServerRefusal(writer http.ResponseWriter, requestID string, err error) bool {
	message := ""
	var coded interface {
		LocalAPICode() string
		LocalAPIMessage() string
	}
	var refused *api.APIError
	switch {
	case errors.As(err, &coded) && coded.LocalAPICode() == RelayQuotaCode:
		message = coded.LocalAPIMessage()
	case errors.As(err, &refused) && refused.Code == TokenScopeCode:
		writeError(writer, http.StatusForbidden, requestID, TokenScopeCode, api.PublicMessage(TokenScopeCode))
		return true
	case errors.As(err, new(*NoEligiblePathError)):
		var waiting *NoEligiblePathError
		errors.As(err, &waiting)
		writeError(writer, http.StatusServiceUnavailable, requestID, NoEligiblePathCode, waiting.Error())
		return true
	case errors.As(err, &refused) && refused.Code == RelayQuotaCode:
		// The control plane refused the connection descriptor at the plan limit (exec, ssh, probe).
		// Its message is the authored one plus the vetted billing link.
		message = refused.Message
	}
	if !safeText(message) {
		return false
	}
	writeError(writer, http.StatusTooManyRequests, requestID, RelayQuotaCode, message)
	return true
}
