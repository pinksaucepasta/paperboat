//go:build windows

package windowssecurity

import (
	"errors"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

// Security review, 2026-11-10: an elevated process must not use its administrator rights in places its user can
// write. Inside the scope the thread is the same user without them; afterwards it is the process again.
func TestWithUnelevatedTokenIsTheSameUserWithoutAdministratorRights(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	processUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	err = WithUnelevatedToken(func() error {
		ran = true
		var token windows.Token
		if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token); err != nil {
			return err
		}
		defer token.Close()
		if member, err := token.IsMember(administrators); err != nil || member {
			t.Errorf("inside the scope: administrator=%v err=%v", member, err)
		}
		if user, err := token.GetTokenUser(); err != nil || !user.User.Sid.Equals(processUser.User.Sid) {
			t.Errorf("inside the scope the user changed: %v", err)
		}
		if err := WithUnelevatedToken(func() error { return nil }); err == nil {
			t.Error("a scope inside an impersonation was accepted; reverting it would restore the process token")
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("unelevated scope: ran=%v err=%v", ran, err)
	}
	var token windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token); !errors.Is(err, windows.ERROR_NO_TOKEN) {
		if err == nil {
			token.Close()
		}
		t.Fatalf("the scope left a token on the thread: %v", err)
	}
}
