package managedssh

import (
	"context"
	"errors"
	"testing"
)

func TestManagedSSHContextErrorPreservesCustomCancellationCauseAndStatus(t *testing.T) {
	cause := errors.New("caller stopped managed SSH")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	err := managedSSHContextError(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("cancellation status or cause lost: %v", err)
	}
}

func TestManagedSSHDiagnosticFailureKeepsCauseWithoutErrorText(t *testing.T) {
	cause := errors.New("private executable path /home/user/.ssh/agent.sock")
	err := managedSSHFailure("command", ErrOpenSSHUnavailable, cause)
	if !errors.Is(err, ErrOpenSSHUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("sentinel/cause lost: %T", err)
	}
	if err.Error() != ErrOpenSSHUnavailable.Error() {
		t.Fatalf("unexpected failure text: %q", err)
	}
	classified, ok := err.(interface {
		DiagnosticStage() string
		DiagnosticCode() string
	})
	if !ok || classified.DiagnosticStage() != "command" || classified.DiagnosticCode() != "managed_ssh_failed" {
		t.Fatalf("missing bounded classification: %#v", err)
	}
}

func TestManagedSSHBoundaryAddsBoundedMetadataAndLeavesCancellationUntouched(t *testing.T) {
	private := errors.New("private executable path /home/user/.ssh/agent.sock")
	err := managedSSHBoundary("listener_bind", private)
	if !errors.Is(err, private) || err.Error() != errManagedSSHOperation.Error() {
		t.Fatalf("boundary did not preserve cause with safe text: %v", err)
	}
	classified, ok := err.(interface {
		DiagnosticStage() string
		DiagnosticCode() string
	})
	if !ok || classified.DiagnosticStage() != "listener_bind" || classified.DiagnosticCode() != "managed_ssh_failed" {
		t.Fatalf("boundary classification missing: %#v", err)
	}
	for _, status := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := managedSSHBoundary("listener_bind", status); got != status {
			t.Fatalf("boundary changed cancellation status %v into %v", status, got)
		}
	}
}
