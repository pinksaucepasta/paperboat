//go:build windows

package shorttemp

import (
	"testing"

	"golang.org/x/sys/windows"
)

// ownByUser makes the current user the directory's owner. An elevated token (an ssh session, a CI runner) creates it owned by
// Administrators, which code that insists on a user-owned private directory rightly refuses; a normal token already owns it.
func ownByUser(t testing.TB, dir string) {
	t.Helper()
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}
