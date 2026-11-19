package state

import (
	"os"
	"path/filepath"
	"testing"
)

// The SDK session 2026.11.16.2 wrote (testdata from that release's Directory.Save) loads after an update.
func TestLoadsSessionWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-sdk-session.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "sdk")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "sdk-session.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// What the previous release wrote was private to its owner: on Windows that is a protected DACL the loader checks.
	for path, isDir := range map[string]bool{dir: true, file: false} {
		if err := securePath(path, isDir); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := OpenDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if session, err := directory.Load(); err != nil || session.ID != "session_01" {
		t.Fatalf("previous release's session = %+v, %v", session, err)
	}
}
