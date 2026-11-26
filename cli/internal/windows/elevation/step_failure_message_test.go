package elevation

import (
	"errors"
	"strings"
	"testing"
)

type causedStep struct{ cause error }

func (e causedStep) Error() string                { return RuntimeInstallFailureMessage("install_commit") }
func (e causedStep) Unwrap() error                { return e.cause }
func (e causedStep) ElevationFailureCode() string { return "install_commit" }

// Windows parity with 83b3408ab: a failed elevated step said only "failed while committing runtime state".
func TestRuntimeStepFailureMessageNamesTheCause(t *testing.T) {
	got := RuntimeStepFailureMessage(causedStep{errors.New("install inventory: lock held")})
	if !strings.HasPrefix(got, RuntimeInstallFailureMessage("install_commit")) || !strings.Contains(got, "install inventory: lock held") {
		t.Fatalf("message = %q", got)
	}
	if RuntimeStepFailureMessage(errors.New("plain")) != "" {
		t.Fatal("an unnamed error got a step message")
	}
}
