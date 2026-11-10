//go:build windows

package windowssecurity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// winpool (GitHub windows-latest; the account is the built-in Administrator, RID 500): `pb install` failed at "apply
// service declaration: context deadline exceeded". The local daemon's service, LocalSystem, refused its own
// installation: the API writes that account's entry as "LA", and the expected ACL was rewritten to match only when
// the checking process itself ran as that account. The check must hold whoever makes it.
func TestAProtectedDACLNamingTheBuiltInAdministratorMatchesWhoeverChecks(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	if !strings.HasPrefix(sid, "S-1-5-21-") {
		t.Skipf("%s has no account domain to name its built-in Administrator", sid)
	}
	administrator := sid[:strings.LastIndexByte(sid, '-')] + "-500"
	access := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + administrator + ")"
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(access)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if matches, err := CheckProtectedDACLMatches(path, access); err != nil || !matches {
		t.Fatalf("by path: matches=%v err=%v", matches, err)
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.READ_CONTROL, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if matches, err := CheckProtectedHandleDACLMatches(handle, access); err != nil || !matches {
		t.Fatalf("by handle: matches=%v err=%v", matches, err)
	}
	// A different ACL still does not match.
	if matches, _ := CheckProtectedHandleDACLMatches(handle, "D:P(A;;FA;;;SY)(A;;FA;;;BA)"); matches {
		t.Fatal("an ACL without the account's entry matched")
	}
}
