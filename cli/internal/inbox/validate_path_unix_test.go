//go:build !windows

package inbox

import (
	"os"
	"strings"
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
}
