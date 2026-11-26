package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUninstallWaitsForActivationThenRefuses(t *testing.T) {
	wait, poll := uninstallActivationWait, uninstallActivationPoll
	t.Cleanup(func() { uninstallActivationWait, uninstallActivationPoll = wait, poll })
	uninstallActivationWait, uninstallActivationPoll = 30*time.Millisecond, time.Millisecond

	calls := 0
	finishes := func() (bool, error) { calls++; return calls < 3, nil }
	if err := waitForUpdateActivation(context.Background(), finishes); err != nil || calls != 3 {
		t.Fatalf("finishing activation: err=%v calls=%d", err, calls)
	}
	if err := waitForUpdateActivation(context.Background(), func() (bool, error) { return false, errors.New("scm") }); err != nil {
		t.Fatalf("unreadable state must not block uninstall: %v", err)
	}
	stuck := func() (bool, error) { return true, nil }
	var rejection commandRejection
	if err := waitForUpdateActivation(context.Background(), stuck); !errors.As(err, &rejection) || rejection.reason != commandRejectUpdateActivating {
		t.Fatalf("running activation: err=%v, want update activating refusal", err)
	}
}
