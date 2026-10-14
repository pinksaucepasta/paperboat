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

func journalRecoveryEligible(err error) bool {
	return errorLeavesMatch(err, ErrJournalCorrupt)
}

func ensurePrivateJournalDirectory(path string) (result error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		info, probeErr := os.Lstat(path)
		if probeErr == nil && info != nil && info.Mode()&os.ModeSymlink != 0 {
			return safeRotationFailure("connector rotation journal directory failed security checks", errors.Join(ErrInvalidConfig, errJournalSecurity, err))
		}
		if probeErr != nil {
			err = errors.Join(err, probeErr)
		}
		return journalOperationFailure(err)
	}
	handle, err := openWindowsJournalHandle(path, true, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := windows.CloseHandle(handle); closeErr != nil {
			result = safeRotationFailure("connector rotation journal directory could not be closed", errors.Join(result, closeErr))
		}
	}()
	trusted, trustErr := windowsJournalHandleOwnerTrusted(handle)
	if trustErr != nil {
		return journalOperationFailure(trustErr)
	}
	if !trusted {
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
		return journalOperationFailure(err)
	}
	runtime.KeepAlive(absolute)
	protected, probeErr := windowssecurity.CheckProtectedHandleDACLMatches(handle, descriptor.String())
	if probeErr != nil {
		return journalOperationFailure(probeErr)
	}
	if !protected {
		return ErrJournalCorrupt
	}
	return nil
}

func readPrivateJournalFile(path string, limit int64) (body []byte, readErr error) {
	handle, err := openWindowsJournalHandle(path, false, windows.GENERIC_READ|windows.READ_CONTROL)
	if errorLeavesMatch(err, os.ErrNotExist) {
		return nil, errJournalFileNotExist
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		closeErr := windows.CloseHandle(handle)
		return nil, safeRotationFailure("connector rotation journal file could not be opened safely", errors.Join(ErrJournalUncertain, closeErr))
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			body = nil
			if readErr == nil {
				readErr = journalOperationFailure(closeErr)
			} else {
				readErr = safeRotationFailure("connector rotation journal file could not be closed", errors.Join(readErr, closeErr))
			}
		}
	}()
	opened, err := file.Stat()
	descriptor, descriptorErr := windowsJournalSecurityDescriptor(false)
	if err != nil || descriptorErr != nil {
		return nil, journalOperationFailure(errors.Join(err, descriptorErr))
	}
	if !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > limit {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	trusted, trustErr := windowsJournalHandleOwnerTrusted(handle)
	if trustErr != nil {
		return nil, journalOperationFailure(trustErr)
	}
	singleLink, linkErr := windowsJournalHandleHasSingleLink(handle)
	if linkErr != nil {
		return nil, journalOperationFailure(linkErr)
	}
	protected, probeErr := windowssecurity.CheckProtectedHandleDACLMatches(handle, descriptor.String())
	if probeErr != nil {
		return nil, journalOperationFailure(probeErr)
	}
	if !trusted || !protected || !singleLink {
		return nil, errors.Join(ErrJournalCorrupt, errJournalSecurity)
	}
	body, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, journalOperationFailure(err)
	}
	if int64(len(body)) != opened.Size() || int64(len(body)) > limit {
		return nil, ErrJournalCorrupt
	}
	return body, nil
}

func writePrivateJournalFile(path string, body []byte) error {
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func windowsJournalSecurityDescriptor(directory bool) (descriptor *windows.SECURITY_DESCRIPTOR, result error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, journalOperationFailure(err)
	}
	defer func() {
		if closeErr := token.Close(); closeErr != nil {
			descriptor = nil
			if result == nil {
				result = journalOperationFailure(closeErr)
			} else {
				result = safeRotationFailure("connector rotation journal security check could not be closed", errors.Join(result, closeErr))
			}
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, journalOperationFailure(err)
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
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
	value, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, journalOperationFailure(err)
	}
	return value, nil
}

func openWindowsJournalHandle(path string, directory bool, access uint32) (windows.Handle, error) {
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		info, probeErr := os.Lstat(path)
		if probeErr == nil && info != nil && info.Mode()&os.ModeSymlink != 0 {
			stateErr := error(ErrJournalCorrupt)
			if directory {
				stateErr = ErrInvalidConfig
			}
			return 0, safeRotationFailure("connector rotation journal path failed security checks", errors.Join(stateErr, errJournalSecurity, err))
		}
		if probeErr != nil {
			err = errors.Join(err, probeErr)
		}
		if errorLeavesMatch(err, os.ErrNotExist) {
			return 0, err
		}
		return 0, journalOperationFailure(err)
	}
	var info windows.ByHandleFileInformation
	if infoErr := windows.GetFileInformationByHandle(handle, &info); infoErr != nil {
		closeErr := windows.CloseHandle(handle)
		return 0, journalOperationFailure(errors.Join(infoErr, closeErr))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		closeErr := windows.CloseHandle(handle)
		stateErr := error(ErrJournalCorrupt)
		if directory {
			stateErr = ErrInvalidConfig
		}
		return 0, safeRotationFailure("connector rotation journal path failed security checks", errors.Join(stateErr, errJournalSecurity, closeErr))
	}
	if (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		closeErr := windows.CloseHandle(handle)
		return 0, safeRotationFailure("connector rotation journal path failed security checks", errors.Join(ErrJournalCorrupt, errJournalSecurity, closeErr))
	}
	return handle, nil
}

func windowsJournalHandleOwnerTrusted(handle windows.Handle) (trusted bool, result error) {
	system, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	admins, adminsErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if systemErr != nil || adminsErr != nil {
		return false, journalOperationFailure(errors.Join(systemErr, adminsErr))
	}
	token, tokenErr := windows.OpenCurrentProcessToken()
	if tokenErr != nil {
		return false, journalOperationFailure(tokenErr)
	}
	defer func() {
		if closeErr := token.Close(); closeErr != nil {
			trusted = false
			if result == nil {
				result = journalOperationFailure(closeErr)
			} else {
				result = safeRotationFailure("connector rotation journal owner check could not be closed", errors.Join(result, closeErr))
			}
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, journalOperationFailure(err)
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return false, ErrInvalidConfig
	}
	for _, sid := range []*windows.SID{system, admins, user.User.Sid} {
		trusted, ownerErr := windowssecurity.CheckHandleOwnerMatchesSID(handle, sid)
		if ownerErr != nil {
			return false, ownerErr
		}
		if trusted {
			return true, nil
		}
	}
	return false, nil
}

func windowsJournalHandleHasSingleLink(handle windows.Handle) (bool, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return false, err
	}
	return info.NumberOfLinks == 1, nil
}
