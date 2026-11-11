//go:build windows

package hostinstall

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Runs elevated (elevated_tests.txt), as `pb install` does: the tree gets the enrolled owner (here this account)
// and its DACL, and its content is untouched.
func TestNativeRepairWindowsTreeOwnerElevated(t *testing.T) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID := tokenUser.User.Sid.String()
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	if err := os.WriteFile(path, []byte("retained state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := repairWindowsTreeACL(root, ownerSID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, path} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || owner.String() != ownerSID {
			t.Fatalf("%s: enrolled owner not assigned", path)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "retained state" {
		t.Fatal("state content changed")
	}
}

// A hard link planted in the tree names a file outside it: the repair refuses it instead of handing that file to
// the enrolled owner.
func TestNativeRepairWindowsTreeRefusesAHardLinkToAnOutsideFile(t *testing.T) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Link(outside, filepath.Join(root, "planted")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := repairWindowsTreeACL(root, tokenUser.User.Sid.String()); err == nil {
		t.Fatal("repair accepted a hard link to a file outside the tree")
	}
	after, err := windows.GetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatalf("the outside file's DACL changed: %s -> %s", before, after)
	}
}
