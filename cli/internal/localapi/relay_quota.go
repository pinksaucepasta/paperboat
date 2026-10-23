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

// Refusal reports whether a local API code is a refusal whose message is shown as it is: the
// connection was refused before anything was sent or started, and retrying does not help.
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
