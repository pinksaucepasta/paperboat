package server

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

type nativePrivateFailure struct {
	stage string
	code  string
	err   error
}

func (*nativePrivateFailure) Error() string { return "native private operation failed" }
func (e *nativePrivateFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}
func (e *nativePrivateFailure) DiagnosticStage() string {
	if e == nil {
		return ""
	}
	return e.stage
}
func (e *nativePrivateFailure) DiagnosticCode() string {
	if e == nil {
		return ""
	}
	return e.code
}

func classifyNativePrivateFailure(stage, code string, err error) error {
	if err == nil || expectedNativePrivateTermination(err) || expectedNativePrivateBindingFailure(err) {
		return err
	}
	var classified interface {
		DiagnosticStage() string
		DiagnosticCode() string
	}
	if errors.As(err, &classified) && classified.DiagnosticStage() != "" && classified.DiagnosticCode() != "" {
		return err
	}
	return &nativePrivateFailure{stage: stage, code: code, err: err}
}

func expectedNativePrivateTermination(err error) bool {
	return allErrorLeavesMatch(err, func(leaf error) bool {
		return leaf == context.Canceled || leaf == io.EOF || leaf == io.ErrClosedPipe || leaf == net.ErrClosed
	})
}

func expectedNativePrivateBindingFailure(err error) bool {
	return allErrorLeavesMatch(err, func(leaf error) bool { return leaf == ErrNativePrivateBinding })
}

func expectedNativePrivateAuthorizationRejection(err error) bool {
	if err == nil || expectedNativePrivateTermination(err) || expectedNativePrivateBindingFailure(err) || expectedCredentialRejection(err) {
		return true
	}
	return allErrorLeavesMatch(err, func(leaf error) bool {
		return leaf == native.ErrInvalid || leaf == streamauth.ErrInvalid || leaf == tailnet.ErrAdmission
	})
}

func reportNativePrivateFailure(ctx context.Context, stage, code string, err error, unexpected bool) {
	if err == nil || expectedNativePrivateTermination(err) || expectedNativePrivateBindingFailure(err) {
		return
	}
	err = classifyNativePrivateFailure(stage, code, err)
	if allErrorLeavesMatch(err, func(leaf error) bool { return leaf == context.DeadlineExceeded }) {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "private_authorization", stage, code, err)
		return
	}
	if unexpected {
		errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "private_authorization", stage, code, err)
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "private_authorization", stage, code, err)
}

var _ interface {
	DiagnosticStage() string
	DiagnosticCode() string
	Unwrap() error
} = (*nativePrivateFailure)(nil)
