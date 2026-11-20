package telemetry

import (
	"context"
	"errors"
	"testing"
)

// A flush barrier the worker already processed wins over a context that ended with it.
func TestAwaitFlushBarrierPrefersAReadyResultOverADoneContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		barrier := make(chan error, 1)
		barrier <- nil
		if err := awaitFlushBarrier(ctx, barrier); err != nil {
			t.Fatalf("a processed barrier was reported as %v", err)
		}
	}
	if err := awaitFlushBarrier(ctx, make(chan error)); !errors.Is(err, context.Canceled) {
		t.Fatalf("an unprocessed barrier on a done context = %v, want context canceled", err)
	}
}
