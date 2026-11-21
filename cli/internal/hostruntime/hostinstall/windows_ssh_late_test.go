package hostinstall

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A winget download that fails twice and then succeeds is retried with backoff and completes; nothing runs once SSH is
// present or after the window.
func TestWindowsLateSSHRetriesUntilObtained(t *testing.T) {
	installed := time.Date(2026, 11, 21, 6, 40, 0, 0, time.UTC)
	ctx := context.Background()
	ready, calls, failures := false, 0, 2
	present := func(context.Context) bool { return ready }
	prepare := func(context.Context) error {
		calls++
		if failures > 0 {
			failures--
			return errors.New("winget download failed")
		}
		ready = true
		return nil
	}
	var r windowsLateSSH
	steps := []struct {
		after   time.Duration
		calls   int
		wantErr bool
	}{
		{time.Minute, 1, true},       // fails; next try in 1 min
		{90 * time.Second, 1, false}, // backoff: not yet
		{2 * time.Minute, 2, true},   // fails; next try in 2 min
		{3 * time.Minute, 2, false},  // backoff: not yet
		{4 * time.Minute, 3, false},  // succeeds
		{5 * time.Minute, 3, false},  // present: nothing to do
	}
	for _, step := range steps {
		err := r.complete(ctx, installed.Add(step.after), installed, present, prepare)
		if calls != step.calls || (err != nil) != step.wantErr {
			t.Fatalf("at +%s: calls=%d err=%v, want calls=%d err=%t", step.after, calls, err, step.calls, step.wantErr)
		}
	}
	if !ready {
		t.Fatal("managed SSH was not completed")
	}
}

func TestWindowsLateSSHStopsAfterWindowAndCapsBackoff(t *testing.T) {
	installed := time.Date(2026, 11, 21, 6, 40, 0, 0, time.UTC)
	calls := 0
	prepare := func(context.Context) error { calls++; return nil } // skipped: still absent
	absent := func(context.Context) bool { return false }
	var r windowsLateSSH
	for now := installed; now.Sub(installed) <= 2*WindowsLateSSHWindow; now = now.Add(time.Minute) {
		_ = r.complete(context.Background(), now, installed, absent, prepare)
	}
	// attempts at +0, +1, +3, +7, +11, +15 min: doubling to the 4 min cap, none after the window
	if calls != 6 || r.delay != windowsLateSSHMaxDelay {
		t.Fatalf("calls=%d delay=%s, want 6 attempts and a %s cap", calls, r.delay, windowsLateSSHMaxDelay)
	}
}
