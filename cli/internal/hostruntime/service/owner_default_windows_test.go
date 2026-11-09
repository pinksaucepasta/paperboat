//go:build windows

package service

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The workload token of an administrator without UAC filtering defaults to Administrators as owner of new files;
// the launcher must make the user the owner (elevated test processes start in exactly that state).
func TestOwnNewObjectsAsUserSetsTheUserAsDefaultOwner(t *testing.T) {
	var primary windows.Token
	access := uint32(windows.TOKEN_QUERY | windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_DUPLICATE)
	if err := windows.DuplicateTokenEx(windows.GetCurrentProcessToken(), access, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
		var process windows.Token
		if openErr := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY, &process); openErr != nil {
			t.Fatal(openErr)
		}
		defer process.Close()
		if err := windows.DuplicateTokenEx(process, access, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary); err != nil {
			t.Fatal(err)
		}
	}
	defer primary.Close()
	if err := ownNewObjectsAsUser(primary); err != nil {
		t.Fatal(err)
	}
	user, err := primary.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	var length uint32
	_ = windows.GetTokenInformation(primary, windows.TokenOwner, nil, 0, &length)
	buffer := make([]byte, length)
	if err := windows.GetTokenInformation(primary, windows.TokenOwner, &buffer[0], length, &length); err != nil {
		t.Fatal(err)
	}
	owner := (*struct{ Owner *windows.SID })(unsafe.Pointer(&buffer[0])).Owner
	if !owner.Equals(user.User.Sid) {
		t.Fatalf("default owner is %s, want the user %s", owner, user.User.Sid)
	}
}
