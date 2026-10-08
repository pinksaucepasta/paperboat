package filetransfer

import (
	"context"
	"errors"
)

type phaseFailure struct {
	stage string
	err   error
}

func (e *phaseFailure) Error() string {
	if e == nil {
		return "file transfer failed"
	}
	switch e.stage {
	case "stream_open":
		return "file transfer stream could not be opened"
	case "delivery":
		if errors.Is(e.err, context.DeadlineExceeded) {
			return "file delivery timed out"
		}
		return "file delivery failed"
	default:
		return "file transfer failed"
	}
}

func (e *phaseFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *phaseFailure) DiagnosticStage() string {
	if e == nil {
		return ""
	}
	return e.stage
}

func (*phaseFailure) DiagnosticCode() string { return "file_transfer_failed" }

func fileTransferPhaseFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	if stage != "stream_open" && stage != "delivery" {
		return err
	}
	return &phaseFailure{stage: stage, err: err}
}

func deliveryContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	err := callerContextError(ctx)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fileTransferPhaseFailure("delivery", err)
	}
	return err
}

func callerContextError(ctx context.Context) error {
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
