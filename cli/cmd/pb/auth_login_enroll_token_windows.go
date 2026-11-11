//go:build windows

package main

import (
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"golang.org/x/sys/windows"
)

func writeLoginEnrollmentToken(path, token string) error {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid := user.User.Sid.String()
	// Pairing accepts exactly SYSTEM and the current user, with no inherited ACEs.
	// Specify the owner too: an elevated process otherwise creates an admin-owned file.
	descriptor := "O:" + sid + "D:P(A;;FA;;;SY)"
	if sid != "S-1-5-18" {
		descriptor += "(A;;FA;;;" + sid + ")"
	}
	return atomicfile.Write(path, []byte(token), atomicfile.Options{
		Mode: 0o600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: descriptor,
	})
}
