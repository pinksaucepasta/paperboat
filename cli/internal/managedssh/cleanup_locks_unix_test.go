//go:build darwin || linux

package managedssh

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestFullUninstallRetiresUnusedSSHLocksAndPreservesForeignFiles(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	if _, err := UninstallOpenSSHConfig(home, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileAuthorizedKeys(home, uid, nil); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(home, ".ssh", "foreign")
	if err := os.WriteFile(foreign, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupUnusedLocks(context.Background(), home, uid); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".paperboat-config.lock", ".paperboat-authorized-keys.lock"} {
		if _, err := os.Lstat(filepath.Join(home, ".ssh", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("owned lock remains", err)
		}
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "preserve" {
		t.Fatal("foreign data changed", err)
	}
}

func TestFullUninstallRejectsActiveOrMalformedSSHLockWithoutPartialRetirement(t *testing.T) {
	for _, kind := range []string{"held", "symlink", "hardlink", "mutated", "mode"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			if err := os.Chmod(home, 0700); err != nil {
				t.Fatal(err)
			}
			ssh := filepath.Join(home, ".ssh")
			if err := os.Mkdir(ssh, 0700); err != nil {
				t.Fatal(err)
			}
			first := filepath.Join(ssh, ".paperboat-config.lock")
			second := filepath.Join(ssh, ".paperboat-authorized-keys.lock")
			for _, p := range []string{first, second} {
				if err := os.WriteFile(p, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "held":
				f, err := os.OpenFile(second, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
			case "symlink":
				if err := os.Rename(second, second+".foreign"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(second+".foreign", second); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(second, second+".foreign"); err != nil {
					t.Fatal(err)
				}
			case "mutated":
				if err := os.WriteFile(second, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(second, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := CleanupUnusedLocks(context.Background(), home, uint32(os.Geteuid())); err == nil {
				t.Fatal("unsafe lock accepted")
			}
			for _, p := range []string{first, second} {
				if _, err := os.Lstat(p); err != nil {
					t.Fatal("lock removed before sibling validation", err)
				}
			}
		})
	}
}
