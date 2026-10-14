//go:build windows

package sessionowner

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
)

func removeRetiredEndpoint(Descriptor) error { return nil }

func validateRetiredDeclaration(path string, _ os.FileInfo, _ Descriptor) error {
	f, err := openPrivate(path, false)
	if err != nil {
		return err
	}
	defer f.Close()
	var st windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &st); err != nil {
		return err
	}
	if st.NumberOfLinks != 1 || st.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return ErrInvalid
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	owner, err := windowssecurity.CheckHandleOwnerMatchesSID(windows.Handle(f.Fd()), system)
	if err != nil || !owner {
		return errors.Join(ErrInvalid, err)
	}
	acl, err := windowssecurity.CheckProtectedHandleDACLMatches(windows.Handle(f.Fd()), "D:P(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil || !acl {
		return errors.Join(ErrInvalid, err)
	}
	return nil
}
