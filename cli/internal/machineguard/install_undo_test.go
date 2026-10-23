//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// Undo restores the exact guard executable an installation replaced (same bytes, checked by hash), stops
// the new guard first and restarts the prior one; Discard keeps the new executable.
func TestUndoRestoresTheExactPriorGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pb")
	prior := []byte("guard 2026.10.23.6")
	if err := os.WriteFile(path, prior, 0o755); err != nil {
		t.Fatal(err)
	}
	install := func() *guardFileRollback {
		rollback, err := snapshotGuardFilesValidated(func(string, string, os.FileInfo) error { return nil }, path)
		if err != nil {
			t.Fatal(err)
		}
		next := filepath.Join(dir, ".next")
		if err := os.WriteFile(next, []byte("guard dev-a9cb0de6"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, path); err != nil {
			t.Fatal(err)
		}
		rollback.record(path)
		return rollback
	}
	var stopped, restarted int
	undo := &Undo{rollback: install(), stop: func(context.Context) error { stopped++; return nil }, restore: func(context.Context) error { restarted++; return nil }}
	if err := undo.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(got) != sha256.Sum256(prior) || stopped != 1 || restarted != 1 {
		t.Fatalf("restored %q (err %v), stopped=%d restarted=%d", got, err, stopped, restarted)
	}
	kept := &Undo{rollback: install()}
	kept.Discard()
	if got, _ := os.ReadFile(path); string(got) != "guard dev-a9cb0de6" {
		t.Fatalf("discard changed the installed guard: %q", got)
	}
	if (*Undo)(nil).Restore(context.Background()) != nil {
		t.Fatal("a nil Undo restores nothing")
	}
}
