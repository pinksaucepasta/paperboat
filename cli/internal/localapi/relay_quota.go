package localapi

import (
	"errors"
	"net/http"
)

// RelayQuotaCode is the local API code for a peer connection that failed because the workspace's
// hosted relay allowance is used up.
const RelayQuotaCode = "bandwidth_quota_exhausted"

// writeRelayQuotaRefusal answers a failed peer operation with the plan-limit refusal when the
// transport error carries one, so the caller can name the limit and the upgrade action instead of a
// generic connectivity failure. It reports whether it answered.
func writeRelayQuotaRefusal(writer http.ResponseWriter, requestID string, err error) bool {
	var coded interface {
		LocalAPICode() string
		LocalAPIMessage() string
	}
	if !errors.As(err, &coded) || coded.LocalAPICode() != RelayQuotaCode || !safeText(coded.LocalAPIMessage()) {
		return false
	}
	writeError(writer, http.StatusTooManyRequests, requestID, RelayQuotaCode, coded.LocalAPIMessage())
	return true
}
