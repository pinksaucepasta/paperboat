//go:build windows

package configsync

import "golang.org/x/sys/windows"

// Windows persists directory metadata through the write-through replacement.
// FlushFileBuffers on a directory handle is not available to ordinary users.
func syncRepositoryDirectory(string) error { return nil }
func renameRepositoryReference(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
