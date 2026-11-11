//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"

	"golang.org/x/sys/windows"
)

const declarationDirectoryDACL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// Rights that let an account plant, swap or re-secure a child of a directory (0x40 is FILE_DELETE_CHILD).
const windowsDirectoryWriteRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | 0x40 |
	windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.GENERIC_ALL | windows.GENERIC_WRITE

// prepareAtomicDirectory makes directory SYSTEM-owned with the canonical SYSTEM/Administrators DACL, like the
// declarations in it: pin cleanup requires SYSTEM custody (winpool-4). Users may create folders under ProgramData,
// so an existing directory is adopted only if SYSTEM or Administrators owns it with exactly that DACL, inside a
// parent no other account can write; everything is checked through handles that do not follow reparse points.
func prepareAtomicDirectory(directory string) error {
	directory = filepath.Clean(directory)
	if err := validateWindowsAncestorDirectories(filepath.Dir(directory), false); err != nil {
		return fmt.Errorf("check the parents of %s: %w", directory, err)
	}
	parent, err := openProtectedWindowsDirectory(filepath.Dir(directory))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(parent)
	if err := checkWindowsParentCustody(parent, filepath.Dir(directory)); err != nil {
		return err
	}
	handle, err := openProtectedWindowsDirectory(directory)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if verifyDeclarationDirectoryCustody(handle, directory) == nil {
		return nil
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	adminOwned, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, administrators)
	if err != nil {
		return fmt.Errorf("read the owner of %s: %w", directory, err)
	}
	exact, err := windowssecurity.CheckProtectedHandleDACLMatches(handle, declarationDirectoryDACL)
	if err != nil {
		return fmt.Errorf("read the permissions of %s: %w", directory, err)
	}
	if !adminOwned || !exact {
		return refusedWindowsDirectory(directory, "is owned by another account or grants access beyond SYSTEM and Administrators")
	}
	return setDeclarationDirectoryCustody(handle, directory)
}

// openProtectedWindowsDirectory creates a missing directory owned by Administrators with the canonical DACL (an
// elevated token's default owner may be the user itself) and opens it without following reparse points. The
// handle carries WRITE_DAC and WRITE_OWNER for the custody transfer.
func openProtectedWindowsDirectory(path string) (windows.Handle, error) {
	descriptor, err := windows.SecurityDescriptorFromString("O:BA" + declarationDirectoryDACL)
	if err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.CreateDirectory(name, &attributes)
	}); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return 0, fmt.Errorf("create %s: %w", path, err)
	}
	runtime.KeepAlive(descriptor)
	var handle windows.Handle
	err = windowssecurity.WithRestorePrivilege(func() (openErr error) {
		handle, openErr = windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		return openErr
	})
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return 0, errors.Join(fmt.Errorf("inspect %s: %w", path, err), windows.CloseHandle(handle))
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return 0, errors.Join(refusedWindowsDirectory(path, "is a junction, link or file instead of a directory"), windows.CloseHandle(handle))
	}
	return handle, nil
}

// checkWindowsParentCustody accepts a parent owned by SYSTEM or Administrators in which no other account may
// create, delete or re-secure entries (C:\ProgramData\Paperboat gives the enrolled user or Users read access only).
func checkWindowsParentCustody(handle windows.Handle, path string) error {
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the permissions of %s: %w", path, err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read the owner of %s: %w", path, err)
	}
	if !trustedWindowsAdministrator(owner) {
		return refusedWindowsDirectory(path, "is owned by an account other than SYSTEM or Administrators")
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return errors.Join(refusedWindowsDirectory(path, "has no access list"), err)
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return fmt.Errorf("read the permissions of %s: %w", path, err)
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask&windowsDirectoryWriteRights != 0 && !trustedWindowsAdministrator(sid) {
			return refusedWindowsDirectory(path, "lets an account other than SYSTEM or Administrators change its contents")
		}
	}
	runtime.KeepAlive(descriptor)
	return nil
}

func trustedWindowsAdministrator(sid *windows.SID) bool {
	return sid != nil && (sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

func setDeclarationDirectoryCustody(handle windows.Handle, path string) error {
	descriptor, err := windows.SecurityDescriptorFromString(declarationDirectoryDACL)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, system, nil, dacl, nil)
	}); err != nil {
		return fmt.Errorf("make %s SYSTEM-owned: %w", path, err)
	}
	runtime.KeepAlive(descriptor)
	return verifyDeclarationDirectoryCustody(handle, path)
}

// verifyDeclarationDirectoryCustody reads the custody back through the handle: a set that did not take must not
// pass as secured.
func verifyDeclarationDirectoryCustody(handle windows.Handle, path string) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	owned, ownerErr := windowssecurity.CheckHandleOwnerMatchesSID(handle, system)
	exact, daclErr := windowssecurity.CheckProtectedHandleDACLMatches(handle, declarationDirectoryDACL)
	if ownerErr != nil || daclErr != nil || !owned || !exact {
		return errors.Join(refusedWindowsDirectory(path, "is not SYSTEM-owned with the SYSTEM and Administrators access list"), ownerErr, daclErr)
	}
	return nil
}

func refusedWindowsDirectory(path, reason string) error {
	return fmt.Errorf("Paperboat refuses %s: it %s; remove it as an administrator and run pb install again: %w", path, reason, ErrInvalidDefinition)
}

// atomicfile.Write uses MOVEFILE_WRITE_THROUGH on Windows. Opening a directory
// with os.Open and calling Sync returns access denied, so there is no second
// POSIX-style directory fsync to perform after declaration removal.
func syncServiceDirectory(path string) error {
	err := validateWindowsDirectoryNoReparse(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func serviceDefinitionSecurityDescriptor() string { return "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)" }

func writeServiceDefinition(path string, data []byte, mode os.FileMode) error {
	return windowssecurity.WithRestorePrivilege(func() error {
		return atomicfile.Write(path, data, atomicfile.Options{Mode: mode, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: serviceDefinitionSecurityDescriptor()})
	})
}

func (i *Installer) publishDefinitionBytes(data []byte) error {
	return atomicWrite(i.definitionPath, data, 0600)
}
