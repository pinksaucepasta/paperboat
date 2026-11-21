//go:build darwin || linux

package hostruntimecmd

import (
	"errors"
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
