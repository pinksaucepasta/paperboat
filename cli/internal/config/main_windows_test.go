//go:build windows

package config

import (
	"fmt"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// An elevated Windows runner gives new files and directories (including t.TempDir) the Administrators group
// as owner, which the credential and shared-lock checks correctly refuse as a foreign owner. Making the
// current user the process's default owner gives tests the ownership a normal session has.
func TestMain(m *testing.M) {
	if err := setDefaultOwnerToCurrentUser(); err != nil {
		fmt.Fprintln(os.Stderr, "set default owner to the current user:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func setDefaultOwnerToCurrentUser() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, &token); err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	owner := struct{ Owner *windows.SID }{Owner: user.User.Sid}
	return windows.SetTokenInformation(token, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner)))
}
