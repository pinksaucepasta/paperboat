//go:build darwin || linux

package hostinstall

import (
	"errors"
	"path/filepath"
	"testing"
)

// A bound install commits inside install-supplied and removes its journal. A rollback that then finds no journal has
// nothing to undo: it says so with a typed error instead of the raw lstat of the removed file.
func TestRollbackWithoutAJournalIsTheTypedNothingPending(t *testing.T) {
	paths := installPaths{journal: filepath.Join(t.TempDir(), "install-journal.json")}
	_, err := readRollbackJournal(paths)
	if !errors.Is(err, ErrNoPendingInstallation) {
		t.Fatalf("err = %v, want ErrNoPendingInstallation", err)
	}
	if message, ok := FailureMessage(err); !ok || message == "" {
		t.Fatalf("the typed error carries no curated message: %q %v", message, ok)
	}
}
