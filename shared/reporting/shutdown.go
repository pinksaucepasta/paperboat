package reporting

import (
	"context"
	"net/http"
	"sync/atomic"
)

// beginSubmit fences only bounded SDK enqueue work, never an operation lifetime.
func (r *Reporter) beginSubmit() bool {
	if r == nil || !r.enabled || r.client == nil {
		return false
	}
	return r.fence.Begin(false)
}

// CloseContext uses one whole shutdown budget. Drained is local SDK completion,
// not a successful HTTP response or proof of Sentry ingestion.
func (r *Reporter) CloseContext(ctx context.Context) {
	if r == nil || !r.enabled || r.client == nil {
		return
	}
	r.fence.Close(ctx, flushTimeout, nil, func(flushCtx context.Context) bool {
		flushed := r.client.FlushWithContext(flushCtx)
		r.client.Close()
		return flushed
	})
}

func (r *Reporter) FlushStatus() string {
	if r == nil || !r.enabled || r.client == nil {
		return "disabled"
	}
	return r.fence.Status()
}

// SDKSubmissionsDropped counts late submissions rejected by the shutdown fence.
// SDK internal buffer losses and failed HTTP attempts are separate quantities.
func (r *Reporter) SDKSubmissionsDropped() uint64 {
	if r == nil {
		return 0
	}
	return r.fence.Dropped()
}

// SDKHTTPFailures counts failed export HTTP attempts, not exact lost signals.
func (r *Reporter) SDKHTTPFailures() uint64 {
	if r == nil {
		return 0
	}
	return r.httpFailures.Load()
}

type sdkHTTPObserver struct {
	next     http.RoundTripper
	failures *atomic.Uint64
}

func (t sdkHTTPObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(req)
	if err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		t.failures.Add(1)
	}
	return response, err
}
