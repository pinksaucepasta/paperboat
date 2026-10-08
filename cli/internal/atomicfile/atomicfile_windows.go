//go:build windows

package atomicfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

type Stage string

const (
	StageValidate Stage = "validate"
	StageCreate   Stage = "create"
	StageWrite    Stage = "write"
	StageOwner    Stage = "owner"
	StageReplace  Stage = "replace"
	StageSyncDir  Stage = "sync_parent"
)

type Error struct {
	Stage Stage
	Path  string
	Err   error
}

func (e *Error) Error() string { return "atomic file write failed" }
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type Options struct {
	Mode     fs.FileMode
	OwnerUID int
	OwnerGID int
	// SecurityDescriptor is an optional SDDL descriptor. If empty, Write uses
	// a protected descriptor for the current owner, SYSTEM, and Administrators.
	// Windows does not have meaningful POSIX UID/GID ownership, so callers must
	// leave OwnerUID and OwnerGID as -1 and use a security descriptor instead.
	SecurityDescriptor string
}

// Write creates a same-directory temporary file with its final protected
// security descriptor in CreateFileW, writes and flushes its contents, then
// replaces the destination with MOVEFILE_WRITE_THROUGH. It deliberately
// rejects POSIX ownership requests: treating a Windows token as a UID would
// be fake security.
func Write(path string, data []byte, options Options) (resultErr error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || options.Mode.Perm() == 0 || options.Mode&^fs.ModePerm != 0 || options.OwnerUID != -1 || options.OwnerGID != -1 {
		return &Error{Stage: StageValidate, Path: path, Err: errors.New("invalid Windows path, mode, or POSIX owner")}
	}
	parent := filepath.Dir(path)
	if err := secureDirectory(parent); err != nil {
		return &Error{Stage: StageValidate, Path: path, Err: err}
	}
	if err := regularNonReparse(path, true); err != nil {
		return &Error{Stage: StageValidate, Path: path, Err: err}
	}
	descriptor := options.SecurityDescriptor
	if descriptor == "" {
		resolved, err := currentOwnerSecurityDescriptor()
		if err != nil {
			return &Error{Stage: StageOwner, Path: path, Err: err}
		}
		descriptor = resolved
	}
	if _, err := windows.SecurityDescriptorFromString(descriptor); err != nil {
		return &Error{Stage: StageValidate, Path: path, Err: err}
	}
	//paperboat:allow-source-policy atomic-replacement owner=atomicfile-windows reason=same-directory-protected-staging
	temporary, temporaryPath, err := createProtectedTemporary(parent, descriptor)
	if err != nil {
		return &Error{Stage: StageCreate, Path: path, Err: err}
	}
	published := false
	defer func() {
		var closeErr, removeErr error
		if temporary != nil {
			closeErr = temporary.Close()
		}
		if !published {
			removeErr = os.Remove(temporaryPath)
		}
		if err := errors.Join(closeErr, removeErr); err != nil {
			resultErr = errors.Join(resultErr, &Error{Stage: StageWrite, Path: path, Err: err})
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return &Error{Stage: StageWrite, Path: path, Err: err}
	}
	if err := temporary.Sync(); err != nil {
		return &Error{Stage: StageWrite, Path: path, Err: err}
	}
	closeErr := temporary.Close()
	temporary = nil
	if err := closeErr; err != nil {
		return &Error{Stage: StageWrite, Path: path, Err: err}
	}
	from, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return &Error{Stage: StageReplace, Path: path, Err: err}
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &Error{Stage: StageReplace, Path: path, Err: err}
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &Error{Stage: StageReplace, Path: path, Err: err}
	}
	published = true
	return nil
}

func createProtectedTemporary(parent, descriptor string) (*os.File, string, error) {
	finalDescriptor, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		return nil, "", err
	}
	creationDescriptorText := descriptor
	finalOwner, _, ownerErr := finalDescriptor.Owner()
	systemOwner, systemOwnerErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err := errors.Join(ownerErr, systemOwnerErr); err != nil {
		return nil, "", err
	}
	transitionToSystem := finalOwner != nil && finalOwner.Equals(systemOwner)
	var administratorsOwner *windows.SID
	if transitionToSystem {
		if !strings.HasPrefix(descriptor, "O:SY") {
			return nil, "", windows.ERROR_INVALID_SECURITY_DESCR
		}
		creationDescriptorText = strings.TrimPrefix(descriptor, "O:SY")
		administratorsOwner, err = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if err != nil {
			return nil, "", err
		}
	}
	creationDescriptor, err := windows.SecurityDescriptorFromString(creationDescriptorText)
	if err != nil {
		return nil, "", err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: creationDescriptor,
	}
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, "", err
		}
		path := filepath.Join(parent, ".paperboat-"+hex.EncodeToString(random))
		pathUTF16, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, "", err
		}
		var handle windows.Handle
		create := func() error {
			var createErr error
			handle, createErr = windows.CreateFile(pathUTF16, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			return createErr
		}
		if transitionToSystem {
			err = windowssecurity.WithRestorePrivilegeAndOwner(administratorsOwner, create)
		} else {
			err = create()
		}
		runtime.KeepAlive(creationDescriptor)
		if err == windows.ERROR_FILE_EXISTS || err == windows.ERROR_ALREADY_EXISTS {
			continue
		}
		if err != nil {
			if handle != 0 {
				closeErr := windows.CloseHandle(handle)
				removeErr := os.Remove(path)
				if errors.Is(removeErr, os.ErrNotExist) {
					removeErr = nil
				}
				err = errors.Join(err, closeErr, removeErr)
			}
			return nil, "", err
		}
		if transitionToSystem {
			ownerMatches, transitionErr := windowssecurity.CheckHandleOwnerMatchesSID(handle, administratorsOwner)
			if transitionErr == nil && !ownerMatches {
				transitionErr = windows.ERROR_INVALID_SECURITY_DESCR
			}
			if transitionErr == nil {
				daclMatches, probeErr := windowssecurity.CheckProtectedHandleDACLMatches(handle, creationDescriptorText)
				transitionErr = probeErr
				if probeErr == nil && !daclMatches {
					transitionErr = windows.ERROR_INVALID_SECURITY_DESCR
				}
			}
			absoluteDescriptor, absoluteErr := finalDescriptor.ToAbsolute()
			transitionErr = errors.Join(transitionErr, absoluteErr)
			var dacl *windows.ACL
			var daclErr error
			if transitionErr == nil {
				dacl, _, daclErr = absoluteDescriptor.DACL()
			}
			transitionErr = errors.Join(transitionErr, daclErr)
			if transitionErr == nil {
				transitionErr = windowssecurity.WithRestorePrivilege(func() error {
					if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
						return err
					}
					runtime.KeepAlive(absoluteDescriptor)
					matches, err := windowssecurity.CheckProtectedHandleDACLMatches(handle, descriptor)
					if err != nil {
						return err
					}
					if !matches {
						return windows.ERROR_INVALID_SECURITY_DESCR
					}
					return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, systemOwner, nil, nil, nil)
				})
				runtime.KeepAlive(absoluteDescriptor)
			}
			if transitionErr == nil {
				matches, probeErr := windowssecurity.CheckHandleOwnerMatchesSID(handle, systemOwner)
				transitionErr = probeErr
				if probeErr == nil && !matches {
					transitionErr = windows.ERROR_INVALID_SECURITY_DESCR
				}
			}
			if transitionErr == nil {
				matches, probeErr := windowssecurity.CheckProtectedHandleDACLMatches(handle, descriptor)
				transitionErr = probeErr
				if probeErr == nil && !matches {
					transitionErr = windows.ERROR_INVALID_SECURITY_DESCR
				}
			}
			if transitionErr != nil {
				return nil, "", errors.Join(transitionErr, windows.CloseHandle(handle), os.Remove(path))
			}
		}
		file := os.NewFile(uintptr(handle), path)
		if file == nil {
			return nil, "", errors.Join(errors.New("wrap protected temporary file handle"), windows.CloseHandle(handle), os.Remove(path))
		}
		return file, path, nil
	}
	return nil, "", windows.ERROR_FILE_EXISTS
}

func currentOwnerSecurityDescriptor() (descriptor string, resultErr error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer func() {
		resultErr = errors.Join(resultErr, token.Close())
		if resultErr != nil {
			descriptor = ""
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		if err == nil {
			err = errors.New("current Windows token has no valid owner SID")
		}
		return "", err
	}
	sid := user.User.Sid.String()
	descriptor = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if sid != "S-1-5-18" {
		descriptor += "(A;;FA;;;" + sid + ")"
	}
	return descriptor, nil
}

func secureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("parent is not a real directory")
		}
		return err
	}
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(nativePath)
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		if err == nil {
			err = errors.New("parent is a reparse point")
		}
		return err
	}
	return nil
}

func regularNonReparse(path string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("destination is not a regular file")
		}
		return err
	}
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(nativePath)
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		if err == nil {
			err = errors.New("destination is a reparse point")
		}
		return err
	}
	return nil
}
