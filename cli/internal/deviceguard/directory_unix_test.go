//go:build linux || darwin

package deviceguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedDirectoryRejectsUnsafeAncestors(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root for actual ownership boundary fixtures")
	}
	root := t.TempDir()
	trusted := filepath.Join(root, "trusted")
	if err := protectedDirectory(filepath.Join(trusted, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(foreign, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(root, "writable")
	if err := os.Mkdir(writable, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0777); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{foreign, writable} {
		target := filepath.Join(parent, "state")
		if err := protectedDirectory(target, 0700); err == nil {
			t.Fatalf("accepted unsafe parent %s", parent)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("mutated unsafe parent before validation: %v", err)
		}
	}
	sticky := filepath.Join(root, "sticky")
	if err := os.Mkdir(sticky, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, os.ModeSticky|0777); err != nil {
		t.Fatal(err)
	}
	if err := protectedDirectory(filepath.Join(sticky, "state"), 0700); err != nil {
		t.Fatalf("root sticky ancestor: %v", err)
	}
	if err := protectedDirectory(sticky, 0700); err == nil {
		t.Fatal("accepted writable sticky leaf")
	}
	rootLink := filepath.Join(root, "trusted-link")
	if err := os.Symlink(sticky, rootLink); err != nil {
		t.Fatal(err)
	}
	if err := protectedDirectory(filepath.Join(rootLink, "linked-state"), 0700); err != nil {
		t.Fatalf("trusted system-style symlink: %v", err)
	}
	foreignLink := filepath.Join(sticky, "foreign-link")
	if err := os.Symlink(trusted, foreignLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(foreignLink, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := protectedDirectory(filepath.Join(foreignLink, "redirected"), 0700); err == nil {
		t.Fatal("accepted foreign symlink")
	}
	if _, err := os.Lstat(filepath.Join(trusted, "redirected")); !os.IsNotExist(err) {
		t.Fatal("foreign symlink target was modified")
	}
	rootForeignLink := filepath.Join(root, "root-link-to-foreign")
	if err := os.Symlink(foreign, rootForeignLink); err != nil {
		t.Fatal(err)
	}
	if err := protectedDirectory(filepath.Join(rootForeignLink, "redirected"), 0700); err == nil {
		t.Fatal("trusted symlink bypassed foreign target ancestor")
	}
}
