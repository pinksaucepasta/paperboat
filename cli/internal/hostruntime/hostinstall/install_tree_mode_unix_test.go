//go:build darwin || linux

package hostinstall

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A strict root umask (077) must not leave new install parents at 0700: the
// enrolled account then cannot reach its binary and pb install fails.
func TestMkdirAllExactModeIgnoresStrictUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	base := t.TempDir()
	leaf := filepath.Join(base, "paperboat", "users", "u1000", "bin")
	if err := mkdirAllExactMode(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(base, "paperboat"), filepath.Join(base, "paperboat", "users"), filepath.Join(base, "paperboat", "users", "u1000"), leaf} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %v, want 0755", path, info.Mode().Perm())
		}
	}
	secret := filepath.Join(base, "state", "updated")
	if err := mkdirAllExactMode(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(secret); info.Mode().Perm() != 0o700 {
		t.Fatalf("leaf mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestEnsureInstallTreeUnderRepairsOnlyRootOwnedBase(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "paperboat")
	if err := os.MkdirAll(filepath.Join(root, "users"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "other")
	if err := ensureInstallTreeUnder(root, outside); err != nil {
		t.Fatalf("outside the base must be a no-op: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureInstallTreeUnder(link, filepath.Join(link, "users")); err == nil {
		t.Fatal("a symlinked base must be refused")
	}
	err := ensureInstallTreeUnder(root, filepath.Join(root, "users", "u1", "bin"))
	if os.Geteuid() != 0 {
		if err == nil {
			t.Fatal("a non-root-owned base must be refused")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "users"), filepath.Join(root, "users", "u1", "bin")} {
		if info, _ := os.Lstat(path); info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %v, want 0755", path, info.Mode().Perm())
		}
	}
}
