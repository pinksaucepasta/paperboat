package localapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// RelayQuotaCode is the local API code for a peer connection that failed because the workspace's
// hosted relay allowance is used up.
const RelayQuotaCode = "bandwidth_quota_exhausted"

// writeRelayQuotaRefusal answers a failed peer operation with the plan-limit refusal when the
// transport error carries one, so the caller can name the limit and the upgrade action instead of a
// generic connectivity failure. It reports whether it answered.
func writeRelayQuotaRefusal(writer http.ResponseWriter, requestID string, err error) bool {
	message := ""
	var coded interface {
		LocalAPICode() string
		LocalAPIMessage() string
	}
	var refused *api.APIError
	switch {
	case errors.As(err, &coded) && coded.LocalAPICode() == RelayQuotaCode:
		message = coded.LocalAPIMessage()
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
