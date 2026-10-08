//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// Create the final ACL with the file: no recovery bytes may reach an inherited
// directory ACL, even briefly. CREATE_NEW also refuses existing reparse points.
func createEnvironmentRecoveryFile(path string) (*os.File, error) {
	parent, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	directory, err := windows.CreateFile(parent, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(directory)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(directory, &info); err != nil {
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, errors.New("ENV recovery output directory is invalid")
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, windows.ERROR_INVALID_SID
	}
	sid := user.User.Sid.String()
	descriptor := "O:" + sid + "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if sid != "S-1-5-18" {
		descriptor += "(A;;FA;;;" + sid + ")"
	}
	sd, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if !windowssecurity.HandleOwnerMatchesSID(handle, user.User.Sid) || !windowssecurity.ProtectedHandleDACLMatches(handle, descriptor) {
		windows.CloseHandle(handle)
		_ = os.Remove(path)
		return nil, windows.ERROR_INVALID_SECURITY_DESCR
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		windows.CloseHandle(handle)
		_ = os.Remove(path)
		return nil, errors.New("open ENV recovery output handle")
	}
	return file, nil
}
