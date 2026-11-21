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
	var r windowsLateSSH
	now := installed.Add(time.Minute)
	if r.pending(now) {
		t.Fatal("a recent install alone should not report a pending SSH retry")
	}
	present := func(context.Context) bool { return ready }
	prepare := func(context.Context) error {
		calls++
		if !r.pending(now) {
			t.Fatal("SSH preparation should report pending while it runs")
		}
		if failures > 0 {
			failures--
			return errors.New("winget download failed")
		}
		ready = true
		return nil
	}
	steps := []struct {
		after   time.Duration
		calls   int
		wantErr bool
	}{
		{time.Minute, 1, false},      // fails quietly; next try in 1 min
		{90 * time.Second, 1, false}, // backoff: not yet
		{2 * time.Minute, 2, false},  // fails quietly; next try in 2 min
		{3 * time.Minute, 2, false},  // backoff: not yet
		{4 * time.Minute, 3, false},  // succeeds
		{5 * time.Minute, 3, false},  // present: nothing to do
	}
	for _, step := range steps {
		now = installed.Add(step.after)
		err := r.complete(ctx, now, installed, present, prepare)
		if calls != step.calls || (err != nil) != step.wantErr {
			t.Fatalf("at +%s: calls=%d err=%v, want calls=%d err=%t", step.after, calls, err, step.calls, step.wantErr)
		}
		if got, want := r.pending(now), !ready; got != want {
			t.Fatalf("at +%s: pending=%t, want %t", step.after, got, want)
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
	reported := 0
	for now := installed; now.Sub(installed) <= 2*WindowsLateSSHWindow; now = now.Add(time.Minute) {
		if r.complete(context.Background(), now, installed, absent, prepare) != nil {
			reported++
		}
	}
	if reported != 1 {
		t.Fatalf("reported %d failures, want only the last attempt's", reported)
	}
	// attempts at +0, +1, +3, +7, +11, +15 min: doubling to the 4 min cap, none after the window
	if calls != 6 || r.delay != windowsLateSSHMaxDelay {
		t.Fatalf("calls=%d delay=%s, want 6 attempts and a %s cap", calls, r.delay, windowsLateSSHMaxDelay)
	}
}

// An update that replaced the generation mid-window ends the attempt without a report or carried-over backoff.
func TestWindowsLateSSHSupersededIsQuiet(t *testing.T) {
	installed := time.Date(2026, 11, 21, 6, 40, 0, 0, time.UTC)
	r := windowsLateSSH{delay: 2 * time.Minute, next: installed}
	err := r.complete(context.Background(), installed.Add(time.Minute), installed, func(context.Context) bool { return false }, func(context.Context) error { return errWindowsLateSSHSuperseded })
	if err != nil || !r.next.IsZero() || r.delay != 0 || r.pending(installed.Add(time.Minute)) {
		t.Fatalf("superseded: err=%v next=%v delay=%v", err, r.next, r.delay)
	}
}

func TestWindowsLateSSHDoesNotReportPendingAfterWindow(t *testing.T) {
	installed := time.Date(2026, 11, 21, 6, 40, 0, 0, time.UTC)
	var r windowsLateSSH
	now := installed.Add(WindowsLateSSHWindow - time.Minute)
	err := r.complete(context.Background(), now, installed, func(context.Context) bool { return false }, func(context.Context) error {
		if r.pending(installed.Add(WindowsLateSSHWindow + time.Second)) {
			t.Fatal("an attempt still running past the retry window must not report pending")
		}
		return errors.New("OpenSSH download failed")
	})
	if err != nil || !r.pending(now) {
		t.Fatalf("retry scheduled at the deadline: pending=%t err=%v", r.pending(now), err)
	}
}
