package native

import (
	"context"
	"errors"
	"testing"
	"time"
)

// dadape direct3b: a relay step that did not honour its context held the
// dial for 10 s. waitRelay must return at the bound regardless.
func TestRelayWaitReturnsAtItsBoundWhenTheRelayStepHangs(t *testing.T) {
	const bound = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	hang := make(chan struct{})
	defer close(hang)
	started := time.Now()
	err := waitRelay(ctx, func() error { <-hang; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > bound+200*time.Millisecond {
		t.Fatalf("hung relay step: err=%v after %v", err, time.Since(started))
	}
}
