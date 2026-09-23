//go:build darwin || linux

package hostinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSuppliedOperationLockExcludesConcurrencyAndRejectsSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.lock")
	first, err := LockSuppliedOperation(path, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := LockSuppliedOperation(path, os.Geteuid()); err == nil {
		second.Close()
		t.Fatal("concurrent installation acquired lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := LockSuppliedOperation(path, os.Geteuid())
	if err != nil {
		t.Fatal("exited caller left stale lock", err)
	}
	second.Close()
	link := path + ".symlink"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if lock, err := LockSuppliedOperation(link, os.Geteuid()); err == nil {
		lock.Close()
		t.Fatal("followed lock symlink")
	}
}
