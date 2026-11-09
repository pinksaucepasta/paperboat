//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func noReport(error) {}

// pbvm1 F14-03: an install killed while the updater kept running left its journals and the candidate's guard,
// and the updater never recovered them (it did only when it started). The running updater looks for them.
func TestARunningUpdaterStopsToRestartOnceADeadWritersStateWasRecovered(t *testing.T) {
	checks := 0
	check := func(context.Context) (bool, error) { checks++; return checks == 3, nil } // the first two looks find nothing
	ranUntilStopped := false
	err := superviseDeadWriters(context.Background(), time.Millisecond, time.Second, check, noReport, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			ranUntilStopped = true
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("the updater ran on after a recovery")
		}
	})
	if !errors.Is(err, errDeadWriterRecovered) || !ranUntilStopped || checks != 3 {
		t.Fatalf("err=%v stopped=%v checks=%d", err, ranUntilStopped, checks)
	}
}

// A failing recovery is reported and retried with backoff while the updater keeps serving; it never restarts it.
func TestAFailingRecoveryWhileRunningIsRetriedWithBackoffAndTheUpdaterKeepsServing(t *testing.T) {
	var checks, reports atomic.Int32
	failed := errors.New("restoring the services failed")
	err := superviseDeadWriters(context.Background(), time.Millisecond, 4*time.Millisecond,
		func(context.Context) (bool, error) { checks.Add(1); return false, failed },
		func(err error) {
			if errors.Is(err, failed) {
				reports.Add(1)
			}
		},
		func(context.Context) error { time.Sleep(60 * time.Millisecond); return nil })
	if err != nil || reports.Load() < 2 || checks.Load() > 40 {
		t.Fatalf("err=%v reports=%d checks=%d", err, reports.Load(), checks.Load())
	}
}

// Nothing to recover leaves the updater running, and its own result stands.
func TestWithNothingToRecoverTheUpdaterRunsAsBefore(t *testing.T) {
	ended := errors.New("updater ended")
	err := superviseDeadWriters(context.Background(), time.Millisecond, time.Second,
		func(context.Context) (bool, error) { return false, nil }, noReport,
		func(context.Context) error { time.Sleep(20 * time.Millisecond); return ended })
	if !errors.Is(err, ended) || errors.Is(err, errDeadWriterRecovered) {
		t.Fatalf("err=%v", err)
	}
}
