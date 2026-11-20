package machineguard

import (
	"context"
	"errors"
	"testing"
)

// A real failure and a canceled ctx are both ready: the failure must win every time (Go picks at random).
func TestServeOutcomePrefersARealFailureOverACanceledContext(t *testing.T) {
	failure := errors.New("apply rules failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		accept, failures := make(chan error, 1), make(chan error, 1)
		failures <- failure
		if err := serveOutcome(ctx, accept, failures); !errors.Is(err, failure) {
			t.Fatalf("failures: serveOutcome = %v, want the failure", err)
		}
		accept <- failure
		if err := serveOutcome(ctx, accept, make(chan error)); !errors.Is(err, failure) {
			t.Fatalf("accept: serveOutcome = %v, want the failure", err)
		}
	}
	if err := serveOutcome(ctx, make(chan error), make(chan error)); !errors.Is(err, context.Canceled) {
		t.Fatalf("no failure: serveOutcome = %v, want context.Canceled", err)
	}
}
