//go:build windows

package hostinstall

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNativeRepairWindowsTreeOwnerAsSystem(t *testing.T) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if tokenUser.User.Sid.String() != "S-1-5-18" {
		t.Skip("requires SYSTEM ownership migration qualification")
	}
	account, err := user.Lookup("Pujan")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "state.json")
	if err := os.WriteFile(path, []byte("retained state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := repairWindowsTreeACL(root, account.Uid); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, path} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || owner.String() != account.Uid {
			t.Fatal("enrolled owner not assigned")
		}
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "retained state" {
		t.Fatal("state content changed")
	}
}
