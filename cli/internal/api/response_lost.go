package api

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// ResponseLostError is a mutation whose request reached the network but whose response did not
// arrive. The server may have applied it, so the caller must check the state before retrying.
type ResponseLostError struct {
	Method, Path string
	Err          error
}

func (e *ResponseLostError) Error() string {
	return "call " + e.Method + " " + e.Path + ": request sent, response lost: " + e.Err.Error()
}
func (e *ResponseLostError) Unwrap() error { return e.Err }

// readRetryAttempts bounds how often an idempotent read is sent after a transport failure.
const readRetryAttempts = 3

const readRetryBase = 250 * time.Millisecond

// send performs one request. A read (GET, HEAD) that fails in transport is sent again up to
// readRetryAttempts times with jittered backoff inside ctx; a mutation is sent once, and a transport
// failure after its request was written is a ResponseLostError.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		var resp *http.Response
		var err error
		for attempt := 0; attempt < readRetryAttempts; attempt++ {
			if attempt > 0 && !sleepContext(req.Context(), readRetryDelay(attempt)) {
				return nil, errors.Join(err, req.Context().Err())
			}
			if resp, err = c.http.Do(req); err == nil || req.Context().Err() != nil {
				return resp, err
			}
		}
		return nil, err
	}
	var written atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { written.Store(true) }}
	resp, err := c.http.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if err != nil && written.Load() && req.Context().Err() == nil {
		return nil, &ResponseLostError{Method: req.Method, Path: req.URL.Path, Err: err}
	}
	return resp, err
}

func readRetryDelay(attempt int) time.Duration {
	base := readRetryBase << (attempt - 1)
	return base + rand.N(base/2)
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
