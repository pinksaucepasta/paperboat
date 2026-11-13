package hostinstall

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCompleteMaintenanceRetryingSucceedsAfterTransientFailures(t *testing.T) {
	calls := 0
	err := completeMaintenanceRetrying(time.Second, time.Millisecond, func(context.Context) error {
		if calls++; calls < 3 {
			return errors.New("hostd lifecycle response could not be read")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err = %v after %d calls, want success on the third", err, calls)
	}
}

func TestCompleteMaintenanceRetryingStillFailsWhenCommitNeverCompletes(t *testing.T) {
	want := ErrInvalidRequest
	calls := 0
	err := completeMaintenanceRetrying(30*time.Millisecond, 10*time.Millisecond, func(context.Context) error { calls++; return want })
	if !errors.Is(err, want) || calls < 2 {
		t.Fatalf("err = %v after %d calls, want the last error after retrying", err, calls)
	}
}
