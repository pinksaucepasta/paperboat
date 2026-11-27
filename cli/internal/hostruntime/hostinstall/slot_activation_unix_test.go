//go:build darwin || linux

package hostinstall

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The inventory's activation time came from the fixed slot's own time, which a rollback rewrites when it stages the
// prior version again: the failed build's root service, started during the failed update, then passed as a draining
// previous generation (device A/B 11-27, linuxpool-6 and macpool-3: inventory ok with two pins live). Only a commit
// records the time, so a failed update and its rollback leave the prior version's record.
func TestSlotActivationIsTheCommitsRecordNotTheSlotsTime(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root: the record is root-owned protected state")
	}
	root := t.TempDir()
	paths := installPaths{metadata: filepath.Join(root, "install-metadata.json")}
	committed := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	if err := recordSlotActivation(paths, "prior", committed); err != nil {
		t.Fatal(err)
	}
	// The rollback staged the prior bytes again just now; the record still says when they were committed.
	if at, ok := slotActivation(paths, "prior"); !ok || !at.Equal(committed) {
		t.Fatalf("activation of the restored prior version = %v %v, want its commit time %v", at, ok, committed)
	}
	// Bytes the record does not name (a signed update's activation, an older install) fall back to the slot's time.
	if _, ok := slotActivation(paths, "candidate"); ok {
		t.Fatal("a record for other bytes was used")
	}
}
