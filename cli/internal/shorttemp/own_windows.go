//go:build windows

package shorttemp

import (
	"testing"

	"golang.org/x/sys/windows"
)

// OwnByUser makes the current user the directory's owner. An elevated token (an ssh session, a CI runner) creates it owned by
// Administrators, which code that insists on a user-owned private directory rightly refuses; a normal token already owns it.
func OwnByUser(t testing.TB, dir string) {
	t.Helper()
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// MachineDir is Dir as an elevated install finds C:\ProgramData\Paperboat: owned by Administrators with a protected
// SYSTEM/Administrators ACL. Machine state code run elevated refuses a parent the user owns, since the user's
// unelevated processes could change it; an unelevated token keeps the user-owned Dir.
func MachineDir(t testing.TB) string {
	t.Helper()
	dir := Dir(t)
	if !windows.GetCurrentProcessToken().IsElevated() {
		return dir
	}
	sd, err := windows.SecurityDescriptorFromString("O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, info, owner, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	return dir
}
