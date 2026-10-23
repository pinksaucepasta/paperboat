package daemoncmd

import (
	"context"
	"errors"
	"testing"
)

func TestTerminalCoordinatorFailureWaitsForRuntimeCleanup(t *testing.T) {
	failure := errors.New("authorization revoked")
	cleanup := errors.New("cleanup failed")
	failures := make(chan error, 1)
	failures <- failure
	cleaned := false
	err := runWithCoordinatorFailure(t.Context(), failures, func(ctx context.Context) error {
		<-ctx.Done()
		cleaned = true
		return errors.Join(context.Canceled, cleanup)
	})
	if !cleaned || !errors.Is(err, failure) || !errors.Is(err, cleanup) {
		t.Fatalf("cleaned=%v err=%v", cleaned, err)
	}
}
func TestRuntimeFailurePropagatesWithoutCoordinatorFailure(t *testing.T) {
	expected := errors.New("runtime failed")
	if err := runWithCoordinatorFailure(t.Context(), nil, func(context.Context) error { return expected }); !errors.Is(err, expected) {
		t.Fatal(err)
	}
}
