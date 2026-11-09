package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An unexpected failure's cause text is kept locally (owner-only), so a bare internal error can be traced.
func TestUnexpectedFailureCauseIsKeptLocallyOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only is an ACL on Windows, checked by atomicfile")
	}
	directory := filepath.Join(t.TempDir(), "diagnostics")
	cause := errors.Join(errors.New("committed transfer offset did not advance"), errors.New("second leaf"))
	if err := recordUnexpectedFailureCause(directory, "send", "support_ref", cause); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, lastUnexpectedFailureFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 || !strings.Contains(string(body), "send support_ref") || !strings.Contains(string(body), "committed transfer offset did not advance") || !strings.Contains(string(body), "second leaf") {
		t.Fatalf("mode %v body %q", info.Mode().Perm(), body)
	}
}

type uninstallOwnedTestFailure struct{ cause error }

func (uninstallOwnedTestFailure) Error() string                                    { return "uninstall could not finish" }
func (e uninstallOwnedTestFailure) Unwrap() error                                  { return e.cause }
func (uninstallOwnedTestFailure) windowsUninstallFailureOwnership() (string, bool) { return "", false }

// An uninstall failure's causes are private: recording it formats none of them, wherever it sits in the chain.
func TestUnexpectedFailureCauseNeverFormatsAnUninstallFailuresCauses(t *testing.T) {
	calls := 0
	private := &commandPrivateError{calls: &calls}
	cleanup := uninstallOperationTestError{operation: "guard_task_remove", cause: private} // its Error panics
	directory := filepath.Join(t.TempDir(), "diagnostics")
	for _, failure := range []error{
		uninstallOwnedTestFailure{cause: errors.Join(private, cleanup)},
		fmt.Errorf("finish command: %w", errors.Join(errors.New("other"), uninstallOwnedTestFailure{cause: cleanup})),
	} {
		if err := recordUnexpectedFailureCause(directory, "uninstall", "support_ref", failure); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(directory, lastUnexpectedFailureFile))
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 || strings.Contains(string(body), "PRIVATE_COMMAND_PAYLOAD") || !strings.Contains(string(body), "uninstall could not finish") {
			t.Fatalf("private cause formatted %d times; record %q", calls, body)
		}
	}
}
