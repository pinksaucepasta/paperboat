package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// A renewal refused for the account's operation cap waits and asks again instead of ending the send (X-37: a 4.7 GB
// send ended with sdk_limit_reached after 20 minutes).
func TestRenewalWaitsOutTheOperationsCap(t *testing.T) {
	ctx := withTimings(t.Context(), func(timings *commandTimings) { timings.renewLimitFloor = time.Millisecond })
	calls := 0
	got, err := retryRenewalOnLimit(ctx, func() (string, error) {
		calls++
		if calls < 4 {
			return "", &api.APIError{Status: http.StatusTooManyRequests, Code: "sdk_limit_reached"}
		}
		return "fresh", nil
	})
	if err != nil || got != "fresh" || calls != 4 {
		t.Fatalf("got %q after %d calls, err %v", got, calls, err)
	}
}

func TestRenewalDoesNotRetryOtherFailures(t *testing.T) {
	calls := 0
	boom := &api.APIError{Status: http.StatusForbidden, Code: "forbidden"}
	_, err := retryRenewalOnLimit(t.Context(), func() (string, error) { calls++; return "", boom })
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("%d calls, err %v", calls, err)
	}
}

func TestRenewalLimitWaitEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := retryRenewalOnLimit(ctx, func() (string, error) {
		return "", &api.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Hour}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the context error with the refusal", err)
	}
	var refusal *api.APIError
	if !errors.As(err, &refusal) {
		t.Fatal("the 429 refusal was dropped")
	}
}
