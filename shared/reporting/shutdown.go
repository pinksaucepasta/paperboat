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
	if !r.submitMu.TryRLock() {
		r.submissionsDropped.Add(1)
		return false
	}
	if r.closed.Load() {
		r.submitMu.RUnlock()
		r.submissionsDropped.Add(1)
		return false
	}
	return true
}

// CloseContext uses one whole shutdown budget. Drained is local SDK completion,
// not a successful HTTP response or proof of Sentry ingestion.
func (r *Reporter) CloseContext(ctx context.Context) {
	if r == nil || !r.enabled || r.client == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	r.closeOnce.Do(func() {
		r.closeDone = make(chan struct{})
		r.closed.Store(true)
		r.flushStatus.Store(1)
		flushCtx, stop := context.WithTimeout(ctx, flushTimeout)
		go func() {
			defer stop()
			defer close(r.closeDone)
			r.submitMu.Lock()
			defer r.submitMu.Unlock()
			flushed := r.client.FlushWithContext(flushCtx)
			r.client.Close()
			if flushed && flushCtx.Err() == nil {
				r.flushStatus.CompareAndSwap(1, 2)
			} else {
				r.flushStatus.Store(3)
			}
		}()
	})
	select {
	case <-r.closeDone:
	case <-waitCtx.Done():
		r.flushStatus.Store(3)
	}
}

func (r *Reporter) FlushStatus() string {
	if r == nil || !r.enabled || r.client == nil {
		return "disabled"
	}
	switch r.flushStatus.Load() {
	case 1:
		return "flushing"
	case 2:
		return "drained"
	case 3:
		return "timed_out"
	default:
		return "not_started"
	}
}

// SDKSubmissionsDropped counts late submissions rejected by the shutdown fence.
// SDK internal buffer losses and failed HTTP attempts are separate quantities.
func (r *Reporter) SDKSubmissionsDropped() uint64 {
	if r == nil {
		return 0
	}
	return r.submissionsDropped.Load()
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
