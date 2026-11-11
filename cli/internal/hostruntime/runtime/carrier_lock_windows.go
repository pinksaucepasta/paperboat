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

// lockCarrierFile takes the serving role's lock: an exclusive LockFileEx on the role file, refused at once while
// another carrier holds it, released when the handle closes (also when the process dies). It is the same role
// lock as Linux and macOS (flock), so an update's standby carrier serves only after the previous one released it.
// As there (owner = this user, mode 0600), the file belongs to the account the carrier runs as (its enrolled owner
// workload): created with a protected DACL for SYSTEM, Administrators and that account, its owner and DACL checked
// on the open handle. A file that fails the check (another build's or account's) is replaced unless a carrier holds
// it open, which leaves it busy.
func lockCarrierFile(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	descriptor := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + user.User.Sid.String() + ")"
	handle, err := createCarrierRoleLock(path, descriptor)
	if err != nil {
		return nil, err
	}
	if err := checkCarrierRoleLock(handle, user.User.Sid, descriptor); err != nil {
		if closeErr := windows.CloseHandle(handle); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		if removeErr := os.Remove(path); removeErr != nil {
			return nil, errors.Join(err, removeErr)
		}
		if handle, err = createCarrierRoleLock(path, descriptor); err != nil {
			return nil, err
		}
		if err := checkCarrierRoleLock(handle, user.User.Sid, descriptor); err != nil {
			return nil, errors.Join(err, windows.CloseHandle(handle))
		}
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return func() error { return windows.CloseHandle(handle) }, nil
}

// carrierLockBusy reports a role lock another carrier holds: the lock itself, or its file kept open.
func carrierLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func createCarrierRoleLock(path, descriptor string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	sd, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		return 0, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &attributes, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	gort.KeepAlive(sd)
	return handle, err
}

func checkCarrierRoleLock(handle windows.Handle, user *windows.SID, descriptor string) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return errors.Join(ErrProductionInvalid, err)
	}
	trusted := []*windows.SID{user}
	for _, known := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(known)
		if err != nil {
			return err
		}
		trusted = append(trusted, sid)
	}
	owned := false
	for _, sid := range trusted {
		matched, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, sid)
		if err != nil {
			return err
		}
		owned = owned || matched
	}
	protected, err := windowssecurity.CheckProtectedHandleDACLMatches(handle, descriptor)
	if err != nil {
		return err
	}
	if !owned || !protected {
		return errors.Join(ErrProductionInvalid, errors.New("the carrier role lock is not this account's file with its protected DACL"))
	}
	return nil
}
