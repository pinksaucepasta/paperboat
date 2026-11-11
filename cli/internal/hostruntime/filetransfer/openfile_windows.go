//go:build windows

package filetransfer

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// openSpoolFile opens a spool file without following a reparse point and refuses anything but a regular file with
// one link, so a swapped .part or .content cannot redirect a write or a verification outside Root. FILE_SHARE_DELETE
// lets Complete rename the file it holds open and then check that the renamed path is still that file.
func openSpoolFile(path string, write bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ)
	if write {
		access = windows.GENERIC_WRITE
	}
	handle, err := windows.CreateFile(name, access|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err == nil && (info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1) {
		err = errors.New("spool file is not a regular file with one link")
	}
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return os.NewFile(uintptr(handle), path), nil
}
