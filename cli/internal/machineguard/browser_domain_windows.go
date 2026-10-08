//go:build windows

package machineguard

import (
	"errors"
	"golang.org/x/sys/windows"
)

func validBrowserDomainOwner(owner string) bool {
	sid, err := windows.StringToSid(owner)
	return err == nil && sid.String() == owner
}
func validateBrowserDomainCaller(owner string) error {
	if !validBrowserDomainOwner(owner) {
		return errors.New("invalid browser domain owner SID")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if user.User.Sid.String() != owner {
		return errors.New("browser domain owner does not match the elevated OS user")
	}
	return nil
}
