package machineguard

import (
	"context"
	"errors"
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
	if guardReadyTimeout < 30*time.Second {
		t.Fatalf("the guard readiness budget is back under the loaded-machine start time: %v", guardReadyTimeout)
	}
}
