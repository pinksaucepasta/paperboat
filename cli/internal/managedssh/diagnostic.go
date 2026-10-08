package managedssh

import (
	"context"
	"errors"
)

var errManagedSSHOperation = errors.New("managed SSH operation failed")

type diagnosticFailure struct {
	stage  string
	public error
	cause  error
}

func (e *diagnosticFailure) Error() string {
	if e == nil || e.public == nil {
		return "managed SSH operation failed"
	}
	return e.public.Error()
}

func (e *diagnosticFailure) Unwrap() []error {
	if e == nil {
		return nil
	}
	var result []error
	if e.public != nil {
		result = append(result, e.public)
	}
	if e.cause != nil {
		result = append(result, e.cause)
	}
	return result
}

func (*diagnosticFailure) DiagnosticCode() string { return "managed_ssh_failed" }

func (e *diagnosticFailure) DiagnosticStage() string {
	if e == nil {
		return ""
	}
	return e.stage
}

func managedSSHFailure(stage string, public, cause error) error {
	if cause == nil {
		return public
	}
	switch stage {
	case "command", "target_connect", "listener_bind", "listener_accept", "component_start", "component_shutdown":
		return &diagnosticFailure{stage: stage, public: public, cause: cause}
	default:
		if public == nil {
			return cause
		}
		return errors.Join(public, cause)
	}
}

// managedSSHBoundary classifies one exported operation without copying raw
// operating-system or remote error text into its user-facing message. Existing
// typed classifications and cancellation semantics remain authoritative.
func managedSSHBoundary(stage string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var classified interface {
		DiagnosticStage() string
		DiagnosticCode() string
	}
	if errors.As(err, &classified) && classified.DiagnosticStage() != "" && classified.DiagnosticCode() != "" {
		return err
	}
	public := error(errManagedSSHOperation)
	switch {
	case errors.Is(err, ErrOpenSSHConfigConflict):
		public = ErrOpenSSHConfigConflict
	case errors.Is(err, ErrOpenSSHUnavailable):
		public = ErrOpenSSHUnavailable
	case errors.Is(err, ErrAgentDenied):
		public = ErrAgentDenied
	}
	return managedSSHFailure(stage, public, err)
}

func managedSSHContextError(ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	status, cause := ctx.Err(), context.Cause(ctx)
	if cause == nil {
		return status
	}
	if errors.Is(cause, status) {
		return cause
	}
	return errors.Join(status, cause)
}
