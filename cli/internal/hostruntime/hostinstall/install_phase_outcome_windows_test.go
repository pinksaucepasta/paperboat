//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"testing"
)

// A phase that finished as the context ended finished: the select alone reported it canceled half of the time and
// aborted the update.
func TestAwaitWindowsInstallPhasePrefersAReadyResultOverADoneContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		result := make(chan error, 1)
		result <- nil
		if err := awaitWindowsInstallPhase(ctx, "start service", result); err != nil {
			t.Fatalf("a finished phase was reported as %v", err)
		}
	}
	if err := awaitWindowsInstallPhase(ctx, "start service", make(chan error)); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("an unfinished phase on a done context = %v, want canceled", err)
	}
}
