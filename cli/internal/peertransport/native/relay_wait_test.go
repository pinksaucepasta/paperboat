package native

import (
	"context"
	"errors"
	"testing"
	"time"
)

// dadape direct3b: a relay step that did not honour its context held the
// move for 10 s. waitRelay must return at the bound regardless.
func TestRelayWaitReturnsAtItsBoundWhenTheRelayStepHangs(t *testing.T) {
	ctx, cancel := relayPrepareContext(context.Background(), false)
	defer cancel()
	hang := make(chan struct{})
	defer close(hang)
	started := time.Now()
	err := waitRelay(ctx, nil, func() error { <-hang; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > carrierRelayAttemptLimit+200*time.Millisecond {
		t.Fatalf("hung relay step: err=%v after %v", err, time.Since(started))
	}
}

// dadape direct2b: the last relay edge must not be cut at the per-edge bound.
func TestLastRelayEdgeWaitsPastThePerEdgeBound(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelParent()
	ctx, cancel := relayPrepareContext(parent, true)
	defer cancel()
	warm := make(chan error, 1)
	go func() { time.Sleep(carrierRelayAttemptLimit + 400*time.Millisecond); warm <- nil }()
	selected := false
	if err := waitRelay(ctx, warm, func() error { selected = true; return nil }); err != nil || !selected {
		t.Fatalf("slow last relay edge: err=%v selected=%v", err, selected)
	}
	ctx, cancel = relayPrepareContext(parent, false)
	defer cancel()
	slow := make(chan error, 1)
	go func() { time.Sleep(carrierRelayAttemptLimit + 400*time.Millisecond); slow <- nil }()
	if err := waitRelay(ctx, slow, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("non-last slow edge not cut at the bound: %v", err)
	}
}
