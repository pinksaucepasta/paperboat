//go:build windows

package enrollment

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
)

func secureIdentityFile(path string, info os.FileInfo, maximum int64) bool {
	matches, err := checkIdentityFile(path, info, maximum)
	return err == nil && matches
}

func checkIdentityFile(path string, info os.FileInfo, maximum int64) (matches bool, resultErr error) {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximum {
		return false, nil
	}
	single, err := checkWindowsSingleLink(path)
	if err != nil || !single {
		return false, err
	}
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(nativePath)
	if err != nil {
		return false, err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, nil
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, token.Close())
		if resultErr != nil {
			matches = false
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return false, nil
	}
	descriptor := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if user.User.Sid.String() != "S-1-5-18" {
		descriptor += "(A;;FA;;;" + user.User.Sid.String() + ")"
	}
	return windowssecurity.CheckProtectedDACLMatches(path, descriptor)
}

func windowsHasSingleLink(path string) bool {
	matches, err := checkWindowsSingleLink(path)
	return err == nil && matches
}

func checkWindowsSingleLink(path string) (matches bool, resultErr error) {
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(nativePath, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, windows.CloseHandle(handle))
		if resultErr != nil {
			matches = false
		}
	}()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return false, err
	}
	return info.NumberOfLinks == 1, nil
}
