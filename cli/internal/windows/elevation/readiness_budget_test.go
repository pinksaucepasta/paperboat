package elevation

import (
	"testing"
	"time"
)

// A first pair does ~50 s of other work (measured on victus and fresh runners), then waits for the host service and
// the per-user daemon: both windows, that work and a margin must fit the activation budget of the elevated operation.
func TestReadinessWindowsFitTheActivationBudget(t *testing.T) {
	const otherWork, margin = 50 * time.Second, 30 * time.Second
	if ReadinessHostdWindow < 45*time.Second || ReadinessDaemonBudget < 60*time.Second {
		t.Fatalf("readiness windows fell back under the measured first-run time: %v + %v", ReadinessHostdWindow, ReadinessDaemonBudget)
	}
	if ReadinessHostdWindow+ReadinessDaemonBudget+otherWork+margin > RuntimeActivationDuration {
		t.Fatalf("%v + %v + %v + %v exceed the activation budget %v", ReadinessHostdWindow, ReadinessDaemonBudget, otherWork, margin, RuntimeActivationDuration)
	}
}

// The readiness probe does not share the install phases' budget (victus used all of it before readiness began):
// it has its own tail, at least both windows, and an install's bridge expiry (recovery + activation, 35 min) holds the activation budget plus it.
func TestReadinessHasItsOwnTailAfterTheInstallPhases(t *testing.T) {
	if ReadinessTail < ReadinessHostdWindow+ReadinessDaemonBudget {
		t.Fatalf("readiness tail %v is shorter than its two windows %v + %v", ReadinessTail, ReadinessHostdWindow, ReadinessDaemonBudget)
	}
	if got := operationDuration(OperationRuntimeService, ActionInstallCommit); got < RuntimeActivationDuration+ReadinessTail {
		t.Fatalf("install-commit expiry %v cannot hold the activation budget plus the readiness tail", got)
	}
}
