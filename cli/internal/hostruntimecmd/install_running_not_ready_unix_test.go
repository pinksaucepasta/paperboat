//go:build darwin || linux

package hostruntimecmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostinstall"
)

// The privileged step prints its failed step as the last "pb: cause:" line; the caller carries it in the error.
func TestNotReadyStepIsTheLastCauseLine(t *testing.T) {
	stderr := "pb: the new version did not become ready; restoring\npb: cause: keychain locked\nnoise\npb: cause: authentication: credential store unreadable\n"
	if got := notReadyStep(stderr); got != "authentication: credential store unreadable" {
		t.Fatalf("step = %q", got)
	}
	if got := notReadyStep("sudo: a password is required"); got != "" {
		t.Fatalf("step without a cause line = %q", got)
	}
	err := error(&hostinstall.UpdateNotReadyError{Step: "x"})
	if !errors.Is(err, hostinstall.ErrUpdateNotReady) {
		t.Fatal("a rollback with a step must still be ErrUpdateNotReady")
	}
}

// A failed privileged install step (the machine guard on a loaded Mac) is named for --json, which shows no stderr.
func TestInstallGuardStepFailureNamesItsStep(t *testing.T) {
	stderr := "pb: Paperboat installation failed: install Paperboat machine guard. Run pb doctor, then retry.\n"
	if got := privilegedFailedStep(stderr); got != "install Paperboat machine guard" {
		t.Fatalf("step = %q", got)
	}
	if got := privilegedFailedStep(stderr + "pb: hint: run pb doctor\n"); got != "install Paperboat machine guard" {
		t.Fatalf("a later pb line replaced the step: %q", got)
	}
	if got := privilegedFailedStep("pb: Paperboat installation failed: \x1b[31m" + strings.Repeat("x", 300) + "\n"); len(got) != 200 || strings.ContainsRune(got, 0x1b) {
		t.Fatalf("step not bounded/printable: %d bytes", len(got))
	}
	if got := privilegedFailedStep("sudo: a password is required\n"); got != "" {
		t.Fatalf("step without a pb line = %q", got)
	}
}
