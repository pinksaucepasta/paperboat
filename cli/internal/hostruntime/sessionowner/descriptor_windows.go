//go:build windows

package sessionowner

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
)

func validateDescriptorCustody(path string, _ os.FileInfo, d Descriptor) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	owner, err := windowssecurity.CheckOwnerMatchesSID(path, system)
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	acl, err := windowssecurity.CheckProtectedDACLMatches(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;"+d.OwnerSID+")")
	if err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if !owner || !acl {
		return ErrInvalid
	}
	return nil
}
