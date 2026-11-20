package main

import (
	"context"
	"errors"
	"testing"
)

// A worker's own failure wins over the server stopping or the context ending at the same moment; a worker that only
// reports the shutdown does not turn it into an error.
func TestCarrierWorkersOutcomeKeepsAWorkerFailure(t *testing.T) {
	sentinel := errors.New("worker failed")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := make(chan struct{})
	close(stopped)
	for range 200 {
		results := make(chan error, 1)
		results <- sentinel
		if err := carrierWorkersOutcome(context.Background(), stopped, results); !errors.Is(err, sentinel) {
			t.Fatalf("a worker failure with the server stopped = %v", err)
		}
		results = make(chan error, 1)
		results <- sentinel
		if err := carrierWorkersOutcome(ctx, make(chan struct{}), results); !errors.Is(err, sentinel) {
			t.Fatalf("a worker failure with the context done = %v", err)
		}
		results = make(chan error, 1)
		results <- context.Canceled
		if err := carrierWorkersOutcome(context.Background(), stopped, results); err != nil {
			t.Fatalf("a worker that only saw the shutdown made it an error: %v", err)
		}
	}
	if err := carrierWorkersOutcome(ctx, make(chan struct{}), make(chan error)); !errors.Is(err, context.Canceled) {
		t.Fatalf("no worker result, context done = %v", err)
	}
}
