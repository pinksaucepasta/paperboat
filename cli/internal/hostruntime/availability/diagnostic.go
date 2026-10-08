package availability

import (
	"reflect"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

const (
	controlRequestStage = "control_request"
	controlRequestCode  = "control_request_failed"
	localGatewayStage   = "local_gateway"
	localGatewayCode    = "local_gateway_failed"
)

// diagnosticFailure keeps the original cause available to local callers and
// the bounded fault projector while exposing only a static message.
type diagnosticFailure struct {
	stage  string
	code   string
	status int
	cause  error
}

func (e *diagnosticFailure) Error() string {
	switch e.stage {
	case controlRequestStage:
		return "availability policy request failed"
	case localGatewayStage:
		return "availability host service request failed"
	default:
		return "availability reconciliation failed"
	}
}

func (e *diagnosticFailure) Unwrap() error           { return e.cause }
func (e *diagnosticFailure) DiagnosticStage() string { return e.stage }
func (e *diagnosticFailure) DiagnosticCode() string  { return e.code }
func (e *diagnosticFailure) DiagnosticStatus() int   { return e.status }

func controlFailure(err error) error {
	if err == nil {
		return nil
	}
	return &diagnosticFailure{stage: controlRequestStage, code: controlRequestCode, status: diagnosticStatus(err), cause: err}
}

func diagnosticStatus(err error) int {
	remaining := []error{err}
	seen := make(map[error]struct{})
	for visited := 0; len(remaining) > 0 && visited < 16; visited++ {
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			continue
		}
		t := reflect.TypeOf(current)
		if t.Comparable() {
			if _, ok := seen[current]; ok {
				continue
			}
			seen[current] = struct{}{}
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			continue
		}
		if status, ok := current.(interface{ DiagnosticStatus() int }); ok {
			if code := status.DiagnosticStatus(); code >= 400 && code <= 599 {
				return code
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) > 16-visited-len(remaining) {
				return 0
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			remaining = append(remaining, wrapped.Unwrap())
		}
	}
	return 0
}

func localFailure(err error) error {
	if err == nil {
		return nil
	}
	return &diagnosticFailure{stage: localGatewayStage, code: localGatewayCode, cause: err}
}

func isObservedCancellation(fault errorreport.Fault) bool {
	return fault.Outcome == "canceled"
}
