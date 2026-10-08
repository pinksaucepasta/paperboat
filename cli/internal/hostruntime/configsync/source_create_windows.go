//go:build windows

package configsync

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CreateSourceFile creates only a new source. Its ACL uses the stable user SID,
// rather than the creator's logon SID, so the enrolled service can read it after
// a new login. Existing files are never opened or repaired.
func CreateSourceFile(path string) (*os.File, error) {
	if !canonicalAbsolutePath(path) {
		return nil, ErrSourceConfigInvalid
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, ErrSourceConfigInvalid
	}
	private, err := currentPrivateFileDescriptor()
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + private.String())
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, ErrSourceConfigInvalid
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
