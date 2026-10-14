//go:build windows

package configsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows resolves ordinary path component casing to its stored spelling.
// Reparse points remain forbidden by checkMappedPlatformPath before this check.
func mappedPlatformPathsEqual(a, b string) bool { return strings.EqualFold(a, b) }

func checkMappedPlatformPath(path string) error {
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrPathRuleInvalid
	}
	return nil
}

// Keep every ancestor open without FILE_SHARE_DELETE throughout IO. Windows
// consequently refuses ancestor rename/reparse replacement until release.
func lockMappedParents(target string, create bool) (func(), error) {
	handles := []windows.Handle{}
	release := func() {
		for i := len(handles) - 1; i >= 0; i-- {
			windows.CloseHandle(handles[i])
		}
	}
	current := filepath.VolumeName(target) + string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(filepath.Dir(target), current), string(filepath.Separator))
	candidates := []string{current}
	for _, part := range parts {
		if part != "" {
			current = filepath.Join(current, part)
			candidates = append(candidates, current)
		}
	}
	for _, path := range candidates {
		handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if create && (errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) {
			if mkdirErr := os.Mkdir(path, 0700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				release()
				return nil, mkdirErr
			}
			handle, err = windows.CreateFile(windows.StringToUTF16Ptr(path), windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		}
		if err != nil {
			release()
			return nil, err
		}
		handles = append(handles, handle)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
			release()
			return nil, err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			release()
			return nil, ErrPathRuleInvalid
		}
	}
	return release, nil
}
