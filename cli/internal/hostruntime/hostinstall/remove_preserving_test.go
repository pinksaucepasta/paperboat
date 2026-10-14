package hostinstall

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRemovePathPreservingCLILeavesOnlyExecutable(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "bin", "pb")
	if err := os.MkdirAll(filepath.Dir(keep), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("approved CLI"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "credential"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "untouched")
	if err := os.WriteFile(sentinel, []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "foreign")); err != nil {
		t.Fatal(err)
	}
	if err := RemovePathPreservingCLI(context.Background(), root, keep); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(keep)
	if err != nil || string(raw) != "approved CLI" {
		t.Fatalf("CLI lost: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("followed foreign symlink", err)
	}
	ok, err := CLIOnlyRemains(context.Background(), root, keep)
	if err != nil || !ok {
		t.Fatalf("unexpected reset leftovers: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "leftover"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ok, err = CLIOnlyRemains(context.Background(), root, keep)
	if err != nil || ok {
		t.Fatal("unexpected payload accepted", err)
	}
	if err := RemovePathPreservingCLI(context.Background(), root, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("uninstall retained root: %v", err)
	}
}
