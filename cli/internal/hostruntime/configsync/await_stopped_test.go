package configsync

import (
	"context"
	"errors"
	"testing"
)

// Both the run's result and an expired ctx are ready: the result must win every time (Go picks at random).
func TestAwaitStoppedPrefersTheResultOverAnExpiredContext(t *testing.T) {
	flushErr := errors.New("flush failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		done := make(chan error, 1)
		done <- flushErr
		if err := awaitStopped(ctx, done); !errors.Is(err, flushErr) {
			t.Fatalf("awaitStopped = %v, want the run's result", err)
		}
	}
	if err := awaitStopped(ctx, make(chan error)); !errors.Is(err, context.Canceled) {
		t.Fatalf("no result: awaitStopped = %v, want context.Canceled", err)
	}
}
