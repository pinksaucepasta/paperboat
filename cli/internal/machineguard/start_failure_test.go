package machineguard

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStartFailureIsRecordedAndCleared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows guard has no control directory")
	}
	socket := filepath.Join(t.TempDir(), "control", "control.sock")
	recordStartFailure(socket, errors.New("machine guard could not enable PF: pfctl failed"))
	if got := readStartFailure(socket); got != "machine guard could not enable PF: pfctl failed" {
		t.Fatalf("recorded failure = %q", got)
	}
	clearStartFailure(socket)
	if got := readStartFailure(socket); got != "" {
		t.Fatalf("cleared failure still reads %q", got)
	}
}
