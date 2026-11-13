//go:build !windows

package inbox

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestValidatePathNamesTheOSCauseOfAnInboxItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a 0500 directory")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := ValidatePath(dir)
	if err == nil || !strings.Contains(err.Error(), "not writable: permission denied") || strings.Contains(err.Error(), ".paperboat-inbox-probe") {
		t.Fatalf("want the OS cause without the probe path, got %v", err)
	}
	// The receiver's HTTP layer classifies a permanent write refusal by these two lookups (file_transfer_http.go).
	var errno syscall.Errno
	if !errors.Is(err, fs.ErrPermission) || !errors.As(err, &errno) {
		t.Fatalf("the cause must stay in the error chain, got %#v", err)
	}
}
