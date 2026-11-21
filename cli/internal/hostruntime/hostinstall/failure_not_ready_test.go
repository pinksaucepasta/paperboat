package hostinstall

import (
	"errors"
	"syscall"
	"testing"
)

func TestNotReadyIfRolledBackNamesOnlyAFinishedRollbackOfAFailedCandidate(t *testing.T) {
	cause := errors.New("service did not start")
	if !errors.Is(notReadyIfRolledBack(cause, true, true), ErrUpdateNotReady) {
		t.Fatal("a failed candidate with a finished rollback was not named")
	}
	for name, got := range map[string]error{
		"candidate never failed": notReadyIfRolledBack(cause, false, true),
		"rollback unfinished":    notReadyIfRolledBack(cause, true, false),
		"disk full":              notReadyIfRolledBack(syscall.ENOSPC, true, true),
	} {
		if errors.Is(got, ErrUpdateNotReady) {
			t.Fatalf("%s was named as a restored update", name)
		}
	}
	if notReadyIfRolledBack(nil, true, true) != nil {
		t.Fatal("success became a failure")
	}
}

// The Windows bridge wraps the message; the step still comes out clean and round-trips.
func TestNotReadyFromMessageDropsTheBridgeWrapper(t *testing.T) {
	step := "local_access_unavailable: deadline_exceeded"
	wrapped := "elevated Windows operation failed (update_not_ready): " + (&UpdateNotReadyError{Step: step}).Error()
	got := NotReadyFromMessage(wrapped)
	if got.Step != step || !errors.Is(got, ErrUpdateNotReady) {
		t.Fatalf("step = %q", got.Step)
	}
	if NotReadyFromMessage("elevated Windows operation failed (update_not_ready): x").Step != "" {
		t.Fatal("a message without a step produced one")
	}
}
