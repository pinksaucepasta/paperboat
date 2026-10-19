package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestBrowserLoginFailureShowsItsOwnRecoveryText(t *testing.T) {
	login := browserLoginFailure{
		message: "Sign-in is saved, but the installed daemon could not refresh. Run pb login again to finish, or pb doctor to check recovery.",
		cause:   &localapi.RemoteError{Code: "service_unavailable", Message: "inventory refresh timed out"},
	}
	for _, err := range []error{login, errors.Join(login, errors.New("cleanup"))} {
		message := commandFailureMessage(err, classifyCommandFailure(err))
		if !strings.Contains(message, "Sign-in is saved") || !strings.Contains(message, "pb login again") {
			t.Fatalf("login recovery text lost: %q", message)
		}
	}
}

type selfReferencingFailure struct{ next *selfReferencingFailure }

func (e *selfReferencingFailure) Error() string { return "cycle" }
func (e *selfReferencingFailure) Unwrap() error { return e.next }

func TestLoginFailureLookupIsBoundedOnCyclicChains(t *testing.T) {
	loop := &selfReferencingFailure{}
	loop.next = loop
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := boundedBrowserLoginFailure(loop); ok {
			t.Error("found a login failure in a cycle")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cyclic error chain was walked without a bound")
	}
}
