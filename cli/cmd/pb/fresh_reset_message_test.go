package main

import (
	"os"
	"path/filepath"
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

// F1-01: an unclean token file path (the macOS installer's TMPDIR "//") must be named, not collapse to "could not finish the command".
func TestFreshResetNamesAnUncleanEnrollmentTokenFilePath(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	clean := freshResetTokenFile(t)
	unclean := filepath.Dir(clean) + "//" + filepath.Base(clean)
	command := freshEnrollmentResetCommand()
	command.SetArgs([]string{"--confirmation", "RESET PAPERBOAT", "--hostname", hostname, "--enrollment-token-file", unclean, "--json"})
	err = command.Execute()
	if err == nil {
		t.Fatal("reset accepted a token file path with a repeated slash")
	}
	for name, message := range map[string]string{
		"text": commandFailureMessage(err, classifyCommandFailure(err)),
		"json": classifyCLIJSONError(err).Message,
	} {
		if !strings.Contains(message, "enrollment token file") || !strings.Contains(message, "repeated slashes") {
			t.Errorf("%s message does not name the token file path: %q", name, message)
		}
	}
}
