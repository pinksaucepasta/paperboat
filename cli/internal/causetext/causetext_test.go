package causetext

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type stepError struct{ cause error }

func (e *stepError) Error() string { return "install runtime services" }
func (e *stepError) Unwrap() error { return e.cause }

func TestTextFollowsAWrapperThatHidesItsCause(t *testing.T) {
	deep := errors.New("the service did not respond")
	err := fmt.Errorf("install supplied runtime: %w", errors.Join(&stepError{cause: errors.Join(&stepError{cause: deep})}))
	if got := Text(err); !strings.Contains(got, "the service did not respond") {
		t.Fatalf("cause missing from %q", got)
	}
}

func TestTextKeepsAWrapperWhoseTextHoldsItsCause(t *testing.T) {
	err := fmt.Errorf("read state: %w", errors.New("file missing"))
	if got := Text(err); got != "read state: file missing" {
		t.Fatalf("got %q", got)
	}
}
