package machineguard

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func TestWaitGuardReadyOutlastsALateStartAndNamesATimeout(t *testing.T) {
	calls := 0
	late := func(context.Context) error {
		calls++
		if calls < 5 {
			return errors.New("pipe not found")
		}
		return nil
	}
	if err := waitGuardReady(context.Background(), 10*time.Second, late); err != nil || calls != 5 {
		t.Fatalf("a guard that starts after four failed probes: err=%v calls=%d", err, calls)
	}
	never := func(context.Context) error { return errors.New("pipe not found") }
	err := waitGuardReady(context.Background(), 250*time.Millisecond, never)
	if err == nil || !strings.Contains(err.Error(), "machine guard did not become ready") || !strings.Contains(err.Error(), "pipe not found") {
		t.Fatalf("a guard that never starts: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitGuardReady(cancelled, time.Minute, never); !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller's cancellation was not honoured: %v", err)
	}
}

// A guard whose first start gets no CPU never answers; a restart does (winpool-6 under one spinner per core).
func TestStartGuardWithRetryRestartsAStarvedStart(t *testing.T) {
	previous, previousAttempts := guardAttemptTimeout, guardStartAttempts
	t.Cleanup(func() { guardAttemptTimeout, guardStartAttempts = previous, previousAttempts })
	guardAttemptTimeout, guardStartAttempts = 150*time.Millisecond, 3 // the test's own budget; production values are asserted below
	restarts := 0
	probe := func(context.Context) error {
		if restarts == 0 {
			return errors.New("pipe not found")
		}
		return nil
	}
	restart := func(context.Context) error { restarts++; return nil }
	if err := startGuardWithRetry(context.Background(), probe, restart); err != nil || restarts != 1 {
		t.Fatalf("a starved first start: err=%v restarts=%d", err, restarts)
	}
	restarts = 0
	never := func(context.Context) error { return errors.New("pipe not found") }
	err := startGuardWithRetry(context.Background(), never, restart)
	if err == nil || restarts != guardStartAttempts-1 || !strings.Contains(err.Error(), "machine guard did not become ready") {
		t.Fatalf("a guard that never starts: err=%v restarts=%d", err, restarts)
	}
	failing := func(context.Context) error { return errors.New("schtasks refused") }
	if err := startGuardWithRetry(context.Background(), never, failing); err == nil || !strings.Contains(err.Error(), "schtasks refused") || !strings.Contains(err.Error(), "pipe not found") {
		t.Fatalf("a failed restart hid a cause: %v", err)
	}
	if previous < 45*time.Second || previousAttempts < 2 { // one cold start under load needs a long uninterrupted attempt
		t.Fatalf("the total guard start budget fell under a minute: %d x %v", previousAttempts, previous)
	}
}

func TestRemoveGuardImageRetriesALockedImageForABoundedTime(t *testing.T) {
	previous := removeGuardFile
	t.Cleanup(func() { removeGuardFile = previous })
	calls := 0
	removeGuardFile = func(string) error {
		calls++
		if calls < 3 {
			return &fs.PathError{Op: "remove", Path: "pb.exe", Err: fs.ErrPermission}
		}
		return nil
	}
	if err := removeGuardImage("pb.exe", 5*time.Second); err != nil || calls != 3 {
		t.Fatalf("a locked image then freed: err=%v calls=%d", err, calls)
	}
	calls = 0
	removeGuardFile = func(string) error { calls++; return &fs.PathError{Op: "remove", Path: "pb.exe", Err: fs.ErrPermission} }
	if err := removeGuardImage("pb.exe", 250*time.Millisecond); !errors.Is(err, fs.ErrPermission) || calls < 2 {
		t.Fatalf("a permanently locked image: err=%v calls=%d", err, calls)
	}
	calls = 0
	removeGuardFile = func(string) error { calls++; return errors.New("disk on fire") }
	if err := removeGuardImage("pb.exe", 5*time.Second); err == nil || calls != 1 {
		t.Fatalf("an unrelated failure was retried: err=%v calls=%d", err, calls)
	}
}
