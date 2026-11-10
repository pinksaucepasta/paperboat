package main

import (
	"errors"
	"io/fs"
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
		if !strings.Contains(message, "enrollment token file") || !strings.Contains(message, "repeated slashes") || !strings.Contains(message, "0600") || !strings.Contains(message, "512 bytes") || !strings.Contains(message, "Windows") {
			t.Errorf("%s message does not name the token file path: %q", name, message)
		}
	}
}

// Review of b1e57bd05: "credentials were retained" is true only when service termination failed; a reset that fails at a
// later step (state already removed) must not tell the user their credentials were kept.
func TestFailedResetSaysCredentialsWereRetainedOnlyWhenServiceTerminationFailed(t *testing.T) {
	ok := func() error { return nil }
	fail := func() error { return syscall.ENOENT }
	for _, tc := range []struct {
		name     string
		cleanup  func() error
		retained bool
		step     string
	}{
		{"runtime purge fails", func() error {
			return performFreshEnrollmentCleanup(nil, ok, ok, ok, fail, ok, ok)
		}, true, "remove system Paperboat runtime"},
		{"daemon stop fails", func() error {
			return performFreshEnrollmentCleanup(nil, fail, ok, ok, ok, ok, ok)
		}, true, "stop local daemon"},
		{"inbox location unproven", func() error {
			return performFreshEnrollmentCleanup(syscall.ENOENT, ok, ok, ok, ok, ok, ok)
		}, true, "resolve Paperboat Inbox preservation"},
		{"SSH configuration fails after state removal", func() error {
			return performFreshEnrollmentCleanup(nil, ok, fail, ok, ok, ok, ok)
		}, false, "remove managed OpenSSH configuration"},
	} {
		err := tc.cleanup()
		if err == nil {
			t.Fatalf("%s: cleanup succeeded", tc.name)
		}
		message := freshResetCleanupError{err: err}.Error()
		if !strings.Contains(message, "Failed: ") || !strings.Contains(message, tc.step) {
			t.Errorf("%s: message does not name the step %q: %q", tc.name, tc.step, message)
		}
		if got := strings.Contains(message, "retained"); got != tc.retained {
			t.Errorf("%s: mentions retained credentials = %v, want %v: %q", tc.name, got, tc.retained, message)
		}
	}
}

// A reset or uninstall that cannot delete a file says which one and why (a sharing violation on Windows named only
// "remove user Paperboat state" and left the user nothing to act on).
func TestFailedUserStateRemovalNamesTheFileThatCouldNotBeRemoved(t *testing.T) {
	ok := func() error { return nil }
	locked := &fs.PathError{Op: "unlinkat", Path: filepath.Join(string(filepath.Separator), "users", "u", "paperboat", "elevation"), Err: errors.New("in use by another process")}
	err := performFreshEnrollmentCleanup(nil, ok, ok, ok, ok, ok, func() error { return errors.Join(locked) })
	message := commandFailureMessage(err, classifyCommandFailure(err))
	for _, want := range []string{"remove user Paperboat state", "Could not remove " + locked.Path, "in use by another process"} {
		if !strings.Contains(message, want) {
			t.Errorf("message lacks %q: %q", want, message)
		}
	}
}
