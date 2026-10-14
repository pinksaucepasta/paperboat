//go:build !windows

package daemonrpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExplicitCleanupRetiresClosedListenerLockWithoutRemovingForeignFiles(t *testing.T) {
	path := testSocket(t)
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatal("normal shutdown must retain lock", err)
	}
	foreign := filepath.Join(filepath.Dir(path), "foreign")
	if err := os.WriteFile(foreign, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupUnusedAddress(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uninstall retained lock", err)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "preserve" {
		t.Fatal("foreign file changed", err)
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if err := cleanupUnusedAddress(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty runtime namespace retained", err)
	}
}

func TestExplicitCleanupRejectsHeldAndMalformedLocks(t *testing.T) {
	for _, kind := range []string{"held", "mode", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			path := testSocket(t)
			lock := path + ".lock"
			if err := os.WriteFile(lock, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "held":
				f, err := os.OpenFile(lock, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			case "mode":
				if err := os.Chmod(lock, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(lock, lock+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(lock+".foreign", lock); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(lock, lock+".foreign"); err != nil {
					t.Fatal(err)
				}
			}
			if err := cleanupUnusedAddress(context.Background(), path); err == nil {
				t.Fatal("unsafe lock accepted")
			}
			if _, err := os.Lstat(lock); err != nil {
				t.Fatal("unsafe lock removed", err)
			}
		})
	}
}

func testSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pb-daemonrpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "daemon.sock")
}

func TestListenerPreservesLiveSocketAndUnrelatedFile(t *testing.T) {
	path := testSocket(t)
	first, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Listen(path); err == nil {
		second.Close()
		t.Fatal("second daemon acquired live socket")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := Listen(path); err == nil {
		listener.Close()
		t.Fatal("removed unrelated file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "preserve" {
		t.Fatal("existing file changed", err)
	}
}
