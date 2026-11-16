package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// ControlPlaneReachable reports whether the server answers a health request within `within`. Any HTTP answer counts:
// the question is only whether Paperboat can be asked at all. A dropped or blackholed path otherwise holds each
// control request for its 30 s timeout, so a command that can run from the host's own rules asks this first. The
// connection it opens stays in the process pool for the requests that follow.
func ControlPlaneReachable(ctx context.Context, baseURL string, within time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/healthz", nil)
	if err != nil {
		return true // a malformed address is reported by the real request, not mistaken for an outage
	}
	response, err := defaultHTTPClient().Do(request)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	_ = response.Body.Close()
	return true
}
