//go:build windows

package managedssh

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

type pinnedWindowsSSHFile struct {
	file   *os.File
	handle windows.Handle
	value  []byte
}

func (f *pinnedWindowsSSHFile) Close() error {
	if f == nil || f.file == nil {
		return nil
	}
	return f.file.Close()
}

// openPinnedWindowsSSHFile opens one ordinary file without delete sharing,
// verifies the final handle path and file identity, and keeps that exact object
// pinned until Close. FILE_FLAG_OPEN_REPARSE_POINT prevents a final-component
// reparse point from being followed while the file is opened.
func openPinnedWindowsSSHFile(path string, extraAccess uint32) (*pinnedWindowsSSHFile, bool, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || filepath.Base(path) == "." || filepath.Base(path) == ".." {
		return nil, false, ErrOpenSSHConfigConflict
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	access := uint32(windows.GENERIC_READ | windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	handle, err := windows.CreateFile(pointer, access|extraAccess, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		windows.CloseHandle(handle)
		return nil, false, err
	}
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || information.NumberOfLinks != 1 || !windowsSSHHandlePathMatches(handle, path) {
		windows.CloseHandle(handle)
		return nil, false, ErrOpenSSHConfigConflict
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		windows.CloseHandle(handle)
		return nil, false, ErrOpenSSHConfigConflict
	}
	value, err := io.ReadAll(io.LimitReader(file, maxOpenSSHConfigSize+1))
	if err != nil || len(value) > maxOpenSSHConfigSize || bytes.IndexByte(value, 0) >= 0 {
		closeErr := file.Close()
		return nil, false, errors.Join(ErrOpenSSHConfigConflict, err, closeErr)
	}
	return &pinnedWindowsSSHFile{file: file, handle: handle, value: value}, true, nil
}

func windowsSSHHandlePathMatches(handle windows.Handle, expected string) bool {
	buffer := make([]uint16, 256)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil || n == 0 {
			return false
		}
		if n < uint32(len(buffer)) {
			actual := windows.UTF16ToString(buffer[:n])
			if strings.HasPrefix(actual, `\\?\UNC\`) {
				actual = `\\` + strings.TrimPrefix(actual, `\\?\UNC\`)
			} else {
				actual = strings.TrimPrefix(actual, `\\?\`)
			}
			actualPath, actualErr := filepath.Abs(actual)
			expectedPath, expectedErr := filepath.Abs(windowssecurity.LongPath(expected))
			return actualErr == nil && expectedErr == nil && strings.EqualFold(filepath.Clean(actualPath), filepath.Clean(expectedPath))
		}
		if n >= 32768 {
			return false
		}
		buffer = make([]uint16, n+1)
	}
}
