package hostruntimecmd

import (
	"errors"
	"strings"
	"testing"
)

type namedStep struct{ step string }

func (e namedStep) Error() string                { return "step " + e.step + " failed: context deadline exceeded" }
func (e namedStep) ElevationFailureCode() string { return e.step }

// A first pair whose host service is still starting must say so, not "host runtime command failed".
func TestRuntimeFailureMessageNamesAnUnelevatedStepFailure(t *testing.T) {
	message := runtimeFailureMessage(errors.Join(namedStep{"install_readiness"}, errors.New("deadline")))
	for _, want := range []string{"still starting", "pb doctor", "same command again"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the readiness failure lacks %q: %q", want, message)
		}
	}
	if strings.Contains(message, "host runtime command failed") {
		t.Fatalf("the generic string came back: %q", message)
	}
	if generic := runtimeFailureMessage(errors.New("something else")); !strings.Contains(generic, "host runtime command failed") {
		t.Fatalf("an unnamed failure lost the generic message: %q", generic)
	}
}
