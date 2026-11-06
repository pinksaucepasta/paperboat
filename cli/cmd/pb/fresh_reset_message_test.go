package main

import (
	"strings"
	"syscall"
	"testing"
)

// F1-01: a failed reset cleanup names the step that failed; it must not collapse to "could not finish the command"
// because the innermost cause is a bare errno (published installer on macOS: operation_failed / local_io).
func TestFailedResetCleanupNamesTheFailedStepInTextAndJSON(t *testing.T) {
	ok := func() error { return nil }
	err := performFreshEnrollmentCleanup(nil, ok, ok, ok, func() error { return syscall.ENOENT }, ok, ok)
	if err == nil {
		t.Fatal("cleanup with a failing runtime purge succeeded")
	}
	for name, message := range map[string]string{
		"text": commandFailureMessage(err, classifyCommandFailure(err)),
		"json": classifyCLIJSONError(err).Message,
	} {
		if !strings.Contains(message, "fresh enrollment reset was incomplete") || !strings.Contains(message, "remove system Paperboat runtime") {
			t.Errorf("%s message does not name the failed reset step: %q", name, message)
		}
	}
}
