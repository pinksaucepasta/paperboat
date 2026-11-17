package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// A renewal that the server answers 429 (the account's live operations or sessions are at their cap: other sends
// and commands hold them until they expire, minutes) must not end a transfer that already moved gigabytes: the
// sender waits out Retry-After and asks again, until the transfer's own recovery window ends the context.
// shortcut: the wait floor and cap below are fixed, move them to the plan limits if they ever differ per account.
var (
	renewLimitFloor = 2 * time.Second
	renewLimitCap   = time.Minute
)

func retryRenewalOnLimit[T any](ctx context.Context, call func() (T, error)) (T, error) {
	for {
		value, err := call()
		var refusal *api.APIError
		if err == nil || !errors.As(err, &refusal) || refusal.Status != http.StatusTooManyRequests {
			return value, err
		}
		wait := min(max(refusal.RetryAfter, renewLimitFloor), renewLimitCap)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return value, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
	}
}
