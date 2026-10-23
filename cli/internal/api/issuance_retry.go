package api

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"
)

// issuanceAttempts and issuanceFirstDelay bound retrying a per-operation descriptor or grant request
// the server answered 503 or 429 (Lane W: 40 parallel exec from one client; 15 exec descriptors got 503
// from a serialization conflict, support_45f4b28f). Each request names its operation ID, so a repeat
// after a refusal cannot apply twice; each attempt is signed afresh.
const (
	issuanceAttempts   = 5
	issuanceFirstDelay = 250 * time.Millisecond
	issuanceMaxDelay   = 2 * time.Second
)

// doIssuance is do for descriptor and grant issuance: a 503 or 429, or an answer lost after the request
// was sent, is retried with jittered backoff inside ctx; any other answer is final.
func (c *Client) doIssuance(ctx context.Context, method, path string, body, out any, checked bool) error {
	delay := issuanceFirstDelay
	for attempt := 1; ; attempt++ {
		err := c.doRequest(ctx, method, path, body, out, nil, checked)
		if err == nil || attempt == issuanceAttempts || !retryableIssuance(err) {
			return err
		}
		if !sleepContext(ctx, delay/2+rand.N(delay/2+1)) {
			return err
		}
		delay = min(2*delay, issuanceMaxDelay)
	}
}

// retryableIssuance reports a refusal or a lost answer that a repeat of the same operation can resolve.
// A lost answer (Lane H: the grant's connection reset, the local gateway answered 502) is safe to repeat
// because the request names its operation.
func retryableIssuance(err error) bool {
	var lost *ResponseLostError
	if errors.As(err, &lost) {
		return true
	}
	var refused *APIError
	return errors.As(err, &refused) && (refused.Status == http.StatusServiceUnavailable || refused.Status == http.StatusTooManyRequests)
}
