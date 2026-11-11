package server

import (
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
)

// A working directory the target refuses is reported with its own code, not as a generic invalid request that the
// client turned into 'the target did not open the connection' (X-17).
func TestExecResultNamesAWorkingDirectoryRefusal(t *testing.T) {
	if got := execResult(nil, execprocess.ErrWorkingDirectory).ErrorCode; got != "exec_cwd_unavailable" {
		t.Fatalf("code = %q", got)
	}
	if got := execResult(nil, errors.New("other")).ErrorCode; got != "invalid_request" {
		t.Fatalf("other errors keep the generic code, got %q", got)
	}
}
