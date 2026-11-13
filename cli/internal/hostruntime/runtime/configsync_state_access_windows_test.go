//go:build windows

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tree whose ACL names only SYSTEM (what a worker of an earlier logon session leaves) cannot be listed or
// removed by its owner; the repair gives the owner access back so the prune of a replaced assignment succeeds.
func TestRepairConfigSyncStateAccessLetsTheOwnerPruneAnOldSessionsTree(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, strings.Repeat("a", 32))
	file := filepath.Join(dir, "pull", "repository", "object")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repairConfigSyncStateAccess(root); _ = os.RemoveAll(dir) })
	for _, path := range []string{file, filepath.Dir(file), filepath.Dir(filepath.Dir(file)), dir} {
		if err := setStateDACL(path, "D:P(A;;FA;;;SY)"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("this token can still list the tree (backup privilege): the denial this test repairs cannot be produced")
	}
	if err := repairConfigSyncStateAccess(root); err != nil {
		t.Fatal(err)
	}
	if err := pruneOrphanedAssignmentState(root, filepath.Join(root, strings.Repeat("b", 32))); err != nil {
		t.Fatalf("prune after the repair: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("old assignment state still there: %v", err)
	}
}
