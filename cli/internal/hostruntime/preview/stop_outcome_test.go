package preview

import (
	"context"
	"errors"
	"testing"
)

// Both the Stop's result and the context are ready when the outcome is judged: the completed Stop wins (the select
// alone picked the context half of the time, and the caller retried or leaked a stopped lease).
func TestAwaitStopPrefersAReadyResultOverADoneContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		done := make(chan error, 1)
		done <- nil
		if err := awaitStop(ctx, done); err != nil {
			t.Fatalf("a completed stop was reported as %v", err)
		}
		failed := make(chan error, 1)
		sentinel := errors.New("stop failed")
		failed <- sentinel
		if err := awaitStop(ctx, failed); !errors.Is(err, sentinel) {
			t.Fatalf("a failed stop was reported as %v", err)
		}
	}
	if err := awaitStop(ctx, make(chan error)); !errors.Is(err, context.Canceled) {
		t.Fatalf("an unfinished stop on a done context = %v, want context canceled", err)
	}
}
