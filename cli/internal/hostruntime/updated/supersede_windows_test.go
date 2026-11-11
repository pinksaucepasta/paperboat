//go:build windows

package updated

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestRestagingRemovesTheSupersededAwaitingCandidateOnly(t *testing.T) {
	layout := service.Layout{ReleasesRoot: t.TempDir()}
	for _, version := range []string{"2026.11.10.7", "2026.11.10.8", "2026.11.10.9"} {
		dir := filepath.Join(layout.ReleasesRoot, "versions", version)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pb.exe"), []byte(version), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(version string) bool {
		_, err := os.Stat(filepath.Join(layout.ReleasesRoot, "versions", version))
		return err == nil
	}
	// .8 is active, .7 was staged and never approved, .9 replaces it.
	if err := removeSupersededWindowsCandidate(layout, "2026.11.10.7", "2026.11.10.8", "2026.11.10.9"); err != nil {
		t.Fatal(err)
	}
	if exists("2026.11.10.7") || !exists("2026.11.10.8") || !exists("2026.11.10.9") {
		t.Fatalf("after restage: .7=%v .8=%v .9=%v, want only the active version and the candidate", exists("2026.11.10.7"), exists("2026.11.10.8"), exists("2026.11.10.9"))
	}
	// Never the active version, never the candidate just staged, nothing when there was no superseded one.
	for _, superseded := range []string{"", "2026.11.10.8", "2026.11.10.9"} {
		if err := removeSupersededWindowsCandidate(layout, superseded, "2026.11.10.8", "2026.11.10.9"); err != nil || !exists("2026.11.10.8") || !exists("2026.11.10.9") {
			t.Fatalf("superseded %q: err=%v, active or candidate removed", superseded, err)
		}
	}
	// A name that is not one release directory is refused and removes nothing.
	for _, bad := range []string{"..", `..\x`, "a/b"} {
		if err := removeSupersededWindowsCandidate(layout, bad, "2026.11.10.8", "2026.11.10.9"); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if !exists("2026.11.10.8") || !exists("2026.11.10.9") {
		t.Fatal("a refused name removed a release")
	}
}
