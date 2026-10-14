//go:build windows

package service

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"path/filepath"
	"strings"
)

// CanonicalizeWindowsRoleDeclaration completes the trusted Administrators-to-
// SYSTEM publication transition. Its caller supplies a protected, verified
// source identity and must prove the current SCM target before calling.
func CanonicalizeWindowsRoleDeclaration(kind, instance string, expected WindowsExecutableIdentity) (resultErr error) {
	if kind != HostdKind && kind != UpdaterKind || !safeWindowsServiceKind(kind, instance) {
		return ErrInvalidDefinition
	}
	path := filepath.Join(windowsServiceDefinitionRoot, windowsServiceName(kind, instance)+".json")
	if err := validateWindowsRegularFileNoReparse(path, false); err != nil {
		return err
	}
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(ptr, windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_OWNER, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return ErrInvalidDefinition
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	matched, err := windowssecurity.CheckProtectedHandleDACLMatches(handle, "D:P(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil || !matched {
		return errors.Join(ErrInvalidDefinition, err)
	}
	sysOwned, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, system)
	if err != nil {
		return err
	}
	if !sysOwned {
		adminOwned, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, admin)
		if err != nil || !adminOwned {
			return errors.Join(ErrInvalidDefinition, err)
		}
	}
	identity, err := VerifyOwnedWindowsRoleExecutable(kind, instance, expected.Executable)
	if err != nil || !strings.EqualFold(identity.Executable, expected.Executable) || identity.SHA256 != expected.SHA256 || identity.Length != expected.Length {
		return errors.Join(ErrInvalidDefinition, err)
	}
	if sysOwned {
		return nil
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, system, nil, nil, nil)
	}); err != nil {
		return err
	}
	matched, err = windowssecurity.CheckHandleOwnerMatchesSID(handle, system)
	if err != nil || !matched {
		return errors.Join(ErrInvalidDefinition, err)
	}
	return nil
}
