//go:build windows

package runtime

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"golang.org/x/sys/windows"
)

func validateIndependentAccount(d sessionowner.Descriptor) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return errors.Join(sessionowner.ErrInvalid, err)
	}
	if user == nil || user.User.Sid == nil || user.User.Sid.String() != d.OwnerSID {
		return sessionowner.ErrInvalid
	}
	return nil
}
