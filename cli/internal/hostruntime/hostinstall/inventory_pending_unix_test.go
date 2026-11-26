//go:build unix

package hostinstall

import (
	"os"
	"path/filepath"
	"testing"
)

// A failed cutover removes the install journal before the lifecycle restore runs; if that restore does not finish,
// the inventory must still see a pending transaction, or it deletes the previous version's pins.
func TestInventoryTreatsAnUnfinishedLifecycleRestoreAsPending(t *testing.T) {
	root := t.TempDir()
	paths := installPaths{root: root, journal: filepath.Join(root, "journal.json"), updateState: filepath.Join(root, "update")}
	if pending, err := pendingInstallRecord(paths); err != nil || pending != "" {
		t.Fatalf("no records: pending=%q err=%v", pending, err)
	}
	journal := filepath.Join(root, "service-lifecycle", "service-lifecycle.json")
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pending, err := pendingInstallRecord(paths); err != nil || pending != journal {
		t.Fatalf("lifecycle journal left by an unfinished restore: pending=%q err=%v", pending, err)
	}
}
