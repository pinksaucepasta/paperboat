package reporting

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

type cancellationCycle struct{}

func (*cancellationCycle) Error() string   { panic("error text must not be evaluated") }
func (e *cancellationCycle) Unwrap() error { return e }

func TestMixedCancellationRetainsFailure(t *testing.T) {
	var typedNil *cancellationCycle
	deep := error(context.Canceled)
	for range 20 {
		deep = fmt.Errorf("private wrapper: %w", deep)
	}
	for _, test := range []struct {
		name     string
		err      error
		cause    string
		canceled bool
	}{
		{"pure", fmt.Errorf("private wrapper: %w", context.Canceled), "context_canceled", true},
		{"repeated_pure", errors.Join(context.Canceled, context.Canceled), "context_canceled", true},
		{"refused", errors.Join(syscall.ECONNREFUSED, context.Canceled), "connection_refused", false},
		{"deadline", errors.Join(context.Canceled, context.DeadlineExceeded), "deadline_exceeded", false},
		{"unknown", errors.Join(context.Canceled, errors.New("PRIVATE_ERROR")), "internal", false},
		{"cycle", errors.Join(context.Canceled, &cancellationCycle{}), "internal", false},
		{"typed_nil", errors.Join(context.Canceled, typedNil), "internal", false},
		{"truncated", errors.Join(context.Canceled, deep), "internal", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fault := projectFault("paperboat-relay", failureDefinitions["service_run"], "support_5b8a43c1-574f-4c56-94e1-a1adf6fc2c83", test.err)
			if fault.Cause != test.cause {
				t.Fatalf("cause=%s want=%s", fault.Cause, test.cause)
			}
			if test.canceled {
				if fault.Outcome != "canceled" || fault.Severity != "info" {
					t.Fatalf("fault=%+v", fault)
				}
			} else if fault.Outcome != "failed" || fault.Severity != "error" {
				t.Fatalf("fault=%+v", fault)
			}
		})
	}
}
