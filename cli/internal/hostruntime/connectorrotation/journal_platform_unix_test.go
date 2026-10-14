//go:build darwin || linux

package connectorrotation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileJournalRejectsPermissiveParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "permissive")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileJournal(filepath.Join(parent, "rotation.json")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFileJournal error=%v, want invalid configuration", err)
	}
}
