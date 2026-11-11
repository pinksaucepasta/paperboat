//go:build !windows

package ownerpath

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
)

func TestCheckAncestorsRefusesAWritableOrLinkedAncestor(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "a", "private")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckAncestors(private, os.Geteuid()); err != nil {
		t.Fatalf("a private tree was refused: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "a"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := CheckAncestors(private, os.Geteuid()); err == nil {
		t.Fatal("a world-writable ancestor was accepted")
	}
	if err := os.Chmod(filepath.Join(root, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "a"), link); err != nil {
		t.Fatal(err)
	}
	if err := CheckAncestors(filepath.Join(link, "private"), os.Geteuid()); err != nil {
		t.Fatalf("this user's own link to a private tree was refused: %v", err)
	}
	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o777); err != nil || os.Chmod(open, 0o777) != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(open, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	openLink := filepath.Join(root, "openlink")
	if err := os.Symlink(open, openLink); err != nil {
		t.Fatal(err)
	}
	if err := CheckAncestors(filepath.Join(openLink, "private", "leaf"), os.Geteuid()); err == nil {
		t.Fatal("a link into a world-writable directory was accepted")
	}
	if err := CheckAncestors("relative/dir", os.Geteuid()); err == nil {
		t.Fatal("a relative path was accepted")
	}
}

// Under umask 002 with user-private groups (the Ubuntu and Fedora default), the user's directories are group
// writable by a group only they belong to: accepted, as Debian's OpenSSH accepts them.
func TestCheckAncestorsAcceptsTheOwnersPrivateGroup(t *testing.T) {
	owner, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		t.Skip(err)
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getegid()))
	if err != nil || group.Name != owner.Username || os.Geteuid() == 0 {
		t.Skip("this account has no user-private primary group")
	}
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o770); err != nil || os.Chmod(parent, 0o770) != nil {
		t.Fatal(err)
	}
	if err := CheckAncestors(filepath.Join(parent, "private"), os.Geteuid()); err != nil {
		t.Fatalf("a parent writable only by the owner's private group was refused: %v", err)
	}
}
