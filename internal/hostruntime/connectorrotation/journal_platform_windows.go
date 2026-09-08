//go:build windows

package connectorrotation

import (
	"errors"
	"io"
	"os"
	"runtime"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

var errJournalFileNotExist = os.ErrNotExist

func ensurePrivateJournalDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	handle, err := openWindowsJournalHandle(path, true, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if !windowsJournalHandleOwnerTrusted(handle) {
		return ErrJournalCorrupt
	}
	descriptor, err := windowsJournalSecurityDescriptor(true)
	if err != nil {
		return err
	}
	absolute, err := descriptor.ToAbsolute()
	if err != nil {
		return err
	}
	dacl, _, err := absolute.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return err
	}
	runtime.KeepAlive(absolute)
	if !windowssecurity.ProtectedHandleDACLMatches(handle, descriptor.String()) {
		return ErrJournalCorrupt
	}
	return nil
}

func readPrivateJournalFile(path string, limit int64) ([]byte, error) {
	handle, err := openWindowsJournalHandle(path, false, windows.GENERIC_READ|windows.READ_CONTROL)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errJournalFileNotExist
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, ErrJournalCorrupt
	}
	defer file.Close()
	opened, err := file.Stat()
	descriptor, descriptorErr := windowsJournalSecurityDescriptor(false)
	if err != nil || descriptorErr != nil || !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > limit {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	if !windowsJournalHandleOwnerTrusted(handle) || !windowssecurity.ProtectedHandleDACLMatches(handle, descriptor.String()) || !windowsJournalHandleHasSingleLink(handle) {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) != opened.Size() || int64(len(body)) > limit {
		return nil, ErrJournalCorrupt
	}
	return body, nil
}

func writePrivateJournalFile(path string, body []byte) error {
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func windowsJournalSecurityDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, ErrInvalidConfig
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	sddl := "D:P(A;" + flags + ";FA;;;SY)(A;" + flags + ";FA;;;BA)"
	if user.User.Sid.String() != "S-1-5-18" {
		sddl += "(A;" + flags + ";FA;;;" + user.User.Sid.String() + ")"
	}
	return windows.SecurityDescriptorFromString(sddl)
}

func openWindowsJournalHandle(path string, directory bool, access uint32) (windows.Handle, error) {
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(handle, &info) != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		_ = windows.CloseHandle(handle)
		return 0, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	return handle, nil
}

func windowsJournalHandleOwnerTrusted(handle windows.Handle) bool {
	system, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	admins, adminsErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	token, tokenErr := windows.OpenCurrentProcessToken()
	if systemErr != nil || adminsErr != nil || tokenErr != nil {
		return false
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	return err == nil && user != nil && user.User.Sid != nil && (windowssecurity.HandleOwnerMatchesSID(handle, system) || windowssecurity.HandleOwnerMatchesSID(handle, admins) || windowssecurity.HandleOwnerMatchesSID(handle, user.User.Sid))
}

func windowsJournalHandleHasSingleLink(handle windows.Handle) bool {
	var info windows.ByHandleFileInformation
	return windows.GetFileInformationByHandle(handle, &info) == nil && info.NumberOfLinks == 1
}
