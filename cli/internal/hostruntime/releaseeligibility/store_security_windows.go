//go:build windows

package releaseeligibility

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// The deferral record is updater-owned state. It is deliberately machine
// scoped: LocalSystem owns it and only LocalSystem and Administrators receive
// full control. Both directory and record ACLs are protected from inheritance
// so an untrusted parent cannot grant access after validation.
const (
	windowsEligibilityDirectoryDACL = "O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	windowsEligibilityRecordDACL    = "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)"
)

// createTemporaryFile creates the staging object with the final protected
// security descriptor in CreateFileW. Using os.CreateTemp here would leave a
// race in which a different local principal could read or replace the record
// before a subsequent ACL call.
func createTemporaryFile(directory, base string) (*os.File, string, error) {
	descriptor, err := windows.SecurityDescriptorFromString(windowsEligibilityRecordDACL)
	if err != nil {
		return nil, "", err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, "", err
		}
		path := filepath.Join(directory, "."+base+".tmp-"+hex.EncodeToString(random))
		pathUTF16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, "", err
		}
		handle, err := windows.CreateFile(pathUTF16, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		runtime.KeepAlive(descriptor)
		if err == windows.ERROR_FILE_EXISTS || err == windows.ERROR_ALREADY_EXISTS {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		file := os.NewFile(uintptr(handle), path)
		if file == nil {
			closeErr := windows.CloseHandle(handle)
			removeErr := os.Remove(path)
			return nil, "", safeStoreFailure("release eligibility staging file could not be owned", ErrUnsafePath, closeErr, removeErr)
		}
		return file, path, nil
	}
	return nil, "", windows.ERROR_FILE_EXISTS
}

func validateParentSecurity(path string, _ os.FileInfo) error {
	real, err := windowsRealDirectory(path)
	if err != nil {
		return safeStoreFailure("release eligibility directory could not be inspected", ErrUnsafePath, err)
	}
	if !real {
		return ErrUnsafePath
	}
	return validateWindowsObjectSecurity(path, windowsEligibilityDirectoryDACL)
}

func validateRecordSecurity(path string, info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafePath
	}
	return validateRecordPath(path)
}

// secureRecordFile applies the protected owner and DACL to both staging and
// final names. It is called before bytes are written and after replacement, so
// os.CreateTemp never exposes an unprotected deferral payload.
func secureRecordFile(path string) error {
	real, err := windowsRealFile(path)
	if err != nil {
		return safeStoreFailure("release eligibility record could not be inspected", ErrUnsafePath, err)
	}
	if !real {
		return ErrUnsafePath
	}
	descriptor, err := windows.SecurityDescriptorFromString(windowsEligibilityRecordDACL)
	if err != nil {
		return safeStoreFailure("release eligibility record security descriptor is unavailable", ErrUnsafePath, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return safeStoreFailure("release eligibility record ACL is unavailable", ErrUnsafePath, err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return safeStoreFailure("release eligibility system owner is unavailable", ErrUnsafePath, err)
	}
	if err := windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, system, nil, dacl, nil)
	}); err != nil {
		return safeStoreFailure("release eligibility record security could not be applied", ErrUnsafePath, err)
	}
	return validateWindowsObjectSecurity(path, windowsEligibilityRecordDACL)
}

func windowsRealDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(encoded)
	if err != nil {
		return false, err
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0, nil
}

func windowsRealFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(encoded)
	if err != nil {
		return false, err
	}
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0, nil
}

// validateRecordPath performs the path-dependent Windows owner/DACL check.
// os.FileInfo does not retain its full path.
func validateRecordPath(path string) error {
	real, err := windowsRealFile(path)
	if err != nil {
		return safeStoreFailure("release eligibility record could not be inspected", ErrUnsafePath, err)
	}
	if !real {
		return ErrUnsafePath
	}
	return validateWindowsObjectSecurity(path, windowsEligibilityRecordDACL)
}

func validateWindowsObjectSecurity(path, expected string) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return safeStoreFailure("release eligibility system owner is unavailable", ErrUnsafePath, err)
	}
	owner, err := windowssecurity.CheckOwnerMatchesSID(path, system)
	if err != nil {
		return safeStoreFailure("release eligibility owner could not be inspected", ErrUnsafePath, err)
	}
	if !owner {
		return ErrUnsafePath
	}
	protected, err := windowssecurity.CheckProtectedDACLMatches(path, expected)
	if err != nil {
		return safeStoreFailure("release eligibility ACL could not be inspected", ErrUnsafePath, err)
	}
	if !protected {
		return ErrUnsafePath
	}
	return nil
}
