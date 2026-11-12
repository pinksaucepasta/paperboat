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
