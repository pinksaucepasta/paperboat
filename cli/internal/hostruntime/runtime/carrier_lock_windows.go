//go:build windows

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	gort "runtime"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
)

// carrierRoleLockDescriptor admits only SYSTEM (the carrier service) and Administrators, so no other local user can
// create, hold or replace the role lock.
const carrierRoleLockDescriptor = "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"

// lockCarrierFile takes the serving role's lock: an exclusive LockFileEx on the role file, refused at once while
// another carrier holds it, released when the handle closes (also when the process dies). It is the same role
// lock as Linux and macOS (flock), so an update's standby carrier serves only after the previous one released it.
// The file is created with carrierRoleLockDescriptor and its owner and DACL are checked on the open handle, as the
// per-generation owner locks are (hostinstall openCarrierProtectedFile).
func lockCarrierFile(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(carrierRoleLockDescriptor)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &attributes, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	gort.KeepAlive(sd)
	if err != nil {
		return nil, err
	}
	if err := checkCarrierRoleLock(handle); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return func() error { return windows.CloseHandle(handle) }, nil
}

func checkCarrierRoleLock(handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return errors.Join(ErrProductionInvalid, err)
	}
	owned := false
	for _, trusted := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(trusted)
		if err != nil {
			return err
		}
		matched, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, sid)
		if err != nil {
			return err
		}
		owned = owned || matched
	}
	protected, err := windowssecurity.CheckProtectedHandleDACLMatches(handle, carrierRoleLockDescriptor)
	if err != nil {
		return err
	}
	if !owned || !protected {
		return errors.Join(ErrProductionInvalid, errors.New("the carrier role lock is not owned by SYSTEM or Administrators with its protected DACL"))
	}
	return nil
}
