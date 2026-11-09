//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"testing"
	"time"
)

// pbvm1 F14-03: an install killed while the updater kept running left its journals and the candidate's guard,
// and the updater never recovered them (it did only when it started). The running updater looks for them.
func TestARunningUpdaterStopsToRestartOnceAnInterruptedInstallWasUndone(t *testing.T) {
	checks := 0
	check := func(context.Context) (bool, error) { checks++; return checks == 3, nil } // the first two polls find nothing
	ranUntilStopped := false
	err := superviseInterruptedInstall(context.Background(), time.Millisecond, check, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			ranUntilStopped = true
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the updater ran on after an undone install")
		}
	})
	if !errors.Is(err, errInterruptedInstallUndone) || !ranUntilStopped || checks != 3 {
		t.Fatalf("err=%v stopped=%v checks=%d", err, ranUntilStopped, checks)
	}
}

// A failed recovery also ends the run: the start path retries it with its own backoff, so no loop here.
func TestAFailingRecoveryWhileRunningStopsTheUpdaterWithItsError(t *testing.T) {
	failed := errors.New("restoring the services failed")
	err := superviseInterruptedInstall(context.Background(), time.Millisecond,
		func(context.Context) (bool, error) { return false, failed },
		func(ctx context.Context) error { <-ctx.Done(); return nil })
	if !errors.Is(err, failed) {
		t.Fatalf("err=%v", err)
	}
}

// Nothing pending (or a live installer) leaves the updater running, and its own result stands.
func TestWithNothingToRecoverTheUpdaterRunsAsBefore(t *testing.T) {
	ended := errors.New("updater ended")
	err := superviseInterruptedInstall(context.Background(), time.Millisecond,
		func(context.Context) (bool, error) { return false, nil },
		func(context.Context) error { time.Sleep(20 * time.Millisecond); return ended })
	if !errors.Is(err, ended) || errors.Is(err, errInterruptedInstallUndone) {
		t.Fatalf("err=%v", err)
	}
}
