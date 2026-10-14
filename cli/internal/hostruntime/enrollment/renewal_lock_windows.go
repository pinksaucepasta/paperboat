//go:build windows

package enrollment

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

const runtimeIdentityRenewalLockFile = ".runtime-identity-renewal.lock"

type runtimeIdentityRenewalLock struct {
	file     *os.File
	region   windows.Overlapped
	acquired bool
}

func openRuntimeIdentityRenewalLock(stateRoot string) (*runtimeIdentityRenewalLock, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, ErrInvalid
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	user, tokenErr := token.GetTokenUser()
	closeTokenErr := token.Close()
	if err := errors.Join(tokenErr, closeTokenErr); err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, ErrInvalid
	}
	if err := validateRuntimeIdentityStateRoot(stateRoot, user.User.Sid); err != nil {
		return nil, err
	}

	path := filepath.Join(stateRoot, runtimeIdentityRenewalLockFile)
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	acl := runtimeIdentityRenewalFileDACL(user.User.Sid.String())
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + acl)
	if err != nil {
		return nil, err
	}
	attributes := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE | windows.READ_CONTROL | windows.WRITE_OWNER)
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	handle, err := windows.CreateFile(nativePath, access, share, attributes, windows.CREATE_NEW, flags, 0)
	runtime.KeepAlive(descriptor)
	if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		handle, err = windows.CreateFile(nativePath, access, share, nil, windows.OPEN_EXISTING, flags, 0)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return nil, errors.Join(ErrInvalid, windows.CloseHandle(handle))
	}
	closeWith := func(cause error) (*runtimeIdentityRenewalLock, error) {
		return nil, errors.Join(cause, file.Close())
	}

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return closeWith(err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.NumberOfLinks != 1 {
		return closeWith(ErrInvalid)
	}
	opened, err := file.Stat()
	if err != nil {
		return closeWith(err)
	}
	named, err := os.Lstat(path)
	if err != nil {
		return closeWith(err)
	}
	if !opened.Mode().IsRegular() || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, named) {
		return closeWith(ErrInvalid)
	}

	if !windowssecurity.ProtectedHandleDACLMatches(handle, acl) {
		return closeWith(ErrInvalid)
	}
	if !windowssecurity.HandleOwnerMatchesSID(handle, user.User.Sid) {
		// Earlier lock creation inherited the administrator or service owner.
		// Converge only this verified file, preserving its inode and lock region.
		trustedOwner := false
		for _, kind := range []windows.WELL_KNOWN_SID_TYPE{windows.WinBuiltinAdministratorsSid, windows.WinLocalSystemSid} {
			sid, err := windows.CreateWellKnownSid(kind)
			if err != nil {
				return closeWith(err)
			}
			trustedOwner = trustedOwner || windowssecurity.HandleOwnerMatchesSID(handle, sid)
		}
		if !trustedOwner {
			return closeWith(ErrInvalid)
		}
		if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
			return closeWith(err)
		}
		if !windowssecurity.HandleOwnerMatchesSID(handle, user.User.Sid) {
			return closeWith(ErrInvalid)
		}
	}
	return &runtimeIdentityRenewalLock{file: file}, nil
}

func validateRuntimeIdentityStateRoot(path string, sid *windows.SID) (resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalid
	}
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(nativePath, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return errors.Join(ErrInvalid, windows.CloseHandle(handle))
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var nativeInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &nativeInfo); err != nil {
		return err
	}
	if nativeInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || nativeInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return ErrInvalid
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(opened, named) {
		return ErrInvalid
	}
	ownerMatches, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, sid)
	if err != nil {
		return err
	}
	if !ownerMatches {
		return ErrInvalid
	}
	return validateRuntimeIdentityStateRootDACL(handle, sid)
}

func validateRuntimeIdentityStateRootDACL(handle windows.Handle, ownerSID *windows.SID) error {
	// Bootstrap may renew identity before the installer protects this dedicated
	// state directory, so a valid pre-install root can still inherit read-only
	// profile ACEs. Require the enrolled owner and reject every untrusted ACE
	// that can mutate the directory or its child entries.
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if descriptor == nil {
		return ErrInvalid
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return ErrInvalid
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil || ace == nil {
			return errors.Join(ErrInvalid, err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, windows.ACCESS_DENIED_ACE_TYPE:
		default:
			// Unknown ACE forms are not needed for the runtime state root and
			// cannot be evaluated safely by this ownership check.
			return ErrInvalid
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !aceSID.IsValid() {
			return ErrInvalid
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if aceSID.Equals(ownerSID) || aceSID.Equals(systemSID) || aceSID.Equals(adminsSID) {
			continue
		}
		if ace.Mask&runtimeIdentityDirectoryMutationRights != 0 {
			return ErrInvalid
		}
	}
	return nil
}

const (
	runtimeIdentityDirectoryDeleteChild    windows.ACCESS_MASK = 0x40
	runtimeIdentityDirectoryMutationRights windows.ACCESS_MASK = windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | runtimeIdentityDirectoryDeleteChild | windows.GENERIC_WRITE | windows.GENERIC_ALL
)

func runtimeIdentityRenewalFileDACL(sid string) string {
	acl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if sid != "S-1-5-18" {
		acl += "(A;;FA;;;" + sid + ")"
	}
	return acl
}

func (lock *runtimeIdentityRenewalLock) Try() (bool, error) {
	err := windows.LockFileEx(windows.Handle(lock.file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.region)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	if err == nil {
		lock.acquired = true
	}
	return err == nil, err
}

func (lock *runtimeIdentityRenewalLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	var unlockErr error
	if lock.acquired {
		unlockErr = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &lock.region)
	}
	return errors.Join(unlockErr, file.Close())
}
