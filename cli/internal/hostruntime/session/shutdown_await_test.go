package session

import (
	"context"
	"errors"
	"testing"
)

// Closes that already finished when the shutdown ctx ended count: with every result in the shutdown is complete (and
// keeps its close errors), whatever ctx says. A random pick between the ready results and the done ctx reported a
// finished shutdown as canceled.
func TestAwaitClosesCountsFinishedClosesWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closeErr := errors.New("close failed")
	for i := range 200 { // a select with both cases ready picks one at random
		results := make(chan error, 3)
		results <- nil
		results <- closeErr
		results <- ErrSessionRunning
		complete, err := awaitCloses(ctx, results, 3)
		if !complete || !errors.Is(err, closeErr) || errors.Is(err, context.Canceled) {
			t.Fatalf("draw %d: finished closes reported complete=%v err=%v", i, complete, err)
		}
	}
	results := make(chan error, 2)
	results <- nil
	complete, err := awaitCloses(ctx, results, 2)
	if complete || !errors.Is(err, context.Canceled) {
		t.Fatalf("an unfinished close reported complete=%v err=%v, want the cancel", complete, err)
	}
}
