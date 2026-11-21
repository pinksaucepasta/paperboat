//go:build windows

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/windows/elevation"
)

func TestSetupOpenSSHFailuresStopSetup(t *testing.T) {
	budget := withTimings(context.Background(), func(timings *commandTimings) { timings.setupOpenSSH = 30 * time.Millisecond })
	unobtainable := &elevation.RemoteError{Code: elevation.CodeOpenSSHUnobtainable, Message: "no package"}
	if err := runSetupOpenSSH(budget, func(context.Context) error { return unobtainable }); !errors.Is(err, elevation.ErrElevatedOperation) || !strings.Contains(err.Error(), "managed SSH") {
		t.Fatalf("an unobtainable package was skipped: %v", err)
	}
	if err := runSetupOpenSSH(budget, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the budget's expiry was skipped: %v", err)
	}
	if err := runSetupOpenSSH(budget, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("a prepared step failed: %v", err)
	}
}
