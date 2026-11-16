//go:build darwin || linux

package hostruntimecmd

import (
	"os"
	"path/filepath"
	"testing"
)

// F13-02: `pb login --fresh` failed "host runtime command failed" because pairing declared the daemon and the
// runtime artifact from the running process's executable, which the reinstall had just removed (releases/pb.rollback).
func TestLiveExecutableIsTheInstalledBinaryOnceTheRunningOneIsGone(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "download", "pb")
	if err := os.MkdirAll(filepath.Dir(running), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(running, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	self := func() (string, error) { return running, nil }
	installed := filepath.Join(dir, "bin", "pb")
	if got, err := liveExecutable(self, installed); err != nil || got != running {
		t.Fatalf("running binary present: got %q, %v", got, err)
	}
	removed := func() (string, error) { return filepath.Join(dir, "releases", "pb.rollback"), nil }
	if got, err := liveExecutable(removed, installed); err != nil || got != installed {
		t.Fatalf("running binary removed: got %q, %v", got, err)
	}
}
