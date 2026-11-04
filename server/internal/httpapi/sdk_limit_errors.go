package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
)

func writeSDKLimitError(w http.ResponseWriter, r *http.Request, err error) bool {
	var limit *planlimits.SDKLimitError
	if !errors.As(err, &limit) {
		return false
	}
	seconds := max(1, int((limit.RetryAfter+time.Second-1)/time.Second))
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeErrorDetails(w, r, http.StatusTooManyRequests, "sdk_limit_reached", "Wait before requesting another session or descriptor, or close an active operation.", map[string]any{"kind": limit.Kind, "outcome": "not_started", "retry_after_seconds": seconds})
	return true
}
