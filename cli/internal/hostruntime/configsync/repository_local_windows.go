//go:build windows

package configsync

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows persists directory metadata through the write-through replacement.
// FlushFileBuffers on a directory handle is not available to ordinary users.
func syncRepositoryDirectory(string) error { return nil }

// syncRepositoryObject flushes a loose object. go-git leaves objects read-only (0444) and FlushFileBuffers needs
// a handle opened for writing, which Windows refuses on a read-only file, so the attribute is lifted around the
// flush and restored (the object is content addressed and private to this repository).
func syncRepositoryObject(path string) (resultErr error) {
	if err := os.Chmod(path, 0o666); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.Chmod(path, 0o444)) }()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

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
