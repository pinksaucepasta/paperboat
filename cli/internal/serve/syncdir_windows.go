//go:build windows

package serve

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func syncDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("durability boundary is not a directory")
		}
		return serveOperationFailure("could not sync selected serve Inbox directory", err)
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil {
		return serveOperationFailure("could not sync selected serve Inbox directory", err)
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return serveOperationFailure("could not sync selected serve Inbox directory", errors.New("durability boundary is a reparse point"))
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return serveOperationFailure("could not sync selected serve Inbox directory", err)
	}
	syncErr := windows.FlushFileBuffers(handle)
	closeErr := windows.CloseHandle(handle)
	return serveOperationFailure("could not sync selected serve Inbox directory", syncErr, closeErr)
}
