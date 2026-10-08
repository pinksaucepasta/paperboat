//go:build windows

package managedssh

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedSSHAgentPipeConflictUsesTypedWindowsErrors(t *testing.T) {
	for _, code := range []error{
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_FILE_EXISTS,
		windows.ERROR_ALREADY_EXISTS,
		windows.ERROR_PIPE_BUSY,
	} {
		wrapped := fmt.Errorf("private pipe path: %w", code)
		if !managedSSHAgentPipeConflict(wrapped) {
			t.Fatalf("typed pipe conflict %v was not classified", code)
		}
		failure := managedSSHFailure("listener_bind", ErrAgentDenied, wrapped)
		if !errors.Is(failure, ErrAgentDenied) || !errors.Is(failure, code) {
			t.Fatalf("pipe conflict did not preserve public and OS causes: %v", failure)
		}
		var staged interface{ DiagnosticStage() string }
		var coded interface{ DiagnosticCode() string }
		if !errors.As(failure, &staged) || staged.DiagnosticStage() != "listener_bind" ||
			!errors.As(failure, &coded) || coded.DiagnosticCode() != "managed_ssh_failed" {
			t.Fatalf("pipe conflict classification missing: %T %v", failure, failure)
		}
		if failure.Error() != ErrAgentDenied.Error() {
			t.Fatalf("pipe conflict exposed private details: %q", failure)
		}
	}
	if managedSSHAgentPipeConflict(errors.New("named pipe exists")) {
		t.Fatal("free-form error text was classified as an existing pipe")
	}
}
