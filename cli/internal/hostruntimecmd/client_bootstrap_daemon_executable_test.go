//go:build darwin || linux

package hostruntimecmd

import (
	"os"
	"path/filepath"
	"testing"
)

// F13-02: `pb login --fresh` failed "host runtime command failed" because the daemon unit was declared from the
// running process's executable, which the reinstall had just removed (ENOENT on releases/pb.rollback).
func TestBootstrapDaemonExecutablePrefersTheInstalledBinary(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "pb")
	removed := func() (string, error) { return filepath.Join(dir, "releases", "pb.rollback"), nil }
	if err := os.WriteFile(installed, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := bootstrapDaemonExecutable(installed, removed); err != nil || got != installed {
		t.Fatalf("installed present: got %q, %v", got, err)
	}
	download := func() (string, error) { return "/tmp/download/pb", nil }
	if got, err := bootstrapDaemonExecutable(filepath.Join(dir, "missing"), download); err != nil || got != "/tmp/download/pb" {
		t.Fatalf("never installed: got %q, %v", got, err)
	}
}
