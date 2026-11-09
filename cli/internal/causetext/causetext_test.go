package causetext

import (
	"errors"
	"fmt"
	"testing"
)

type stepError struct{ cause error }

func (e *stepError) Error() string { return "install runtime services" }
func (e *stepError) Unwrap() error { return e.cause }

func TestTextFollowsAWrapperThatHidesItsCause(t *testing.T) {
	deep := errors.New("the service did not respond")
	err := fmt.Errorf("install supplied runtime: %w", errors.Join(&stepError{cause: errors.Join(&stepError{cause: deep})}))
	if got := Text(err); got != "install supplied runtime: install runtime services: the service did not respond" {
		t.Fatalf("got %q, want the step names and the cause once each", got)
	}
}

func TestTextKeepsEachJoinedCauseUnderItsStep(t *testing.T) {
	err := &stepError{cause: errors.Join(errors.New("first"), fmt.Errorf("probe: %w", errors.New("second")))}
	if got := Text(err); got != "install runtime services: first; install runtime services: probe: second" {
		t.Fatalf("got %q", got)
	}
}

func TestTextKeepsAWrapperWhoseTextHoldsItsCause(t *testing.T) {
	err := fmt.Errorf("read state: %w", errors.New("file missing"))
	if got := Text(err); got != "read state: file missing" {
		t.Fatalf("got %q", got)
	}
}
