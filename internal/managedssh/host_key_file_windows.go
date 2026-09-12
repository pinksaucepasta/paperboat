//go:build windows

package managedssh

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func openOwnedPublicFile(path string, _ uint32) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("host public key path: %w", ErrHostKeyInventory)
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.NumberOfLinks != 1 || !windowsSSHHandlePathMatches(handle, path) {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("host public key file identity: %w", ErrHostKeyInventory)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		windows.CloseHandle(handle)
		return nil, errors.New("open SSH host public key handle")
	}
	return file, nil
}
