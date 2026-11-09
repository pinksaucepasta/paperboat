//go:build windows

package atomicfile

import "golang.org/x/sys/windows"

// ErrNotDirectory is the cause of a path that runs through a file. It is ERROR_DIRECTORY, because syscall.ENOTDIR
// is ERROR_PATH_NOT_FOUND on Windows and so reads as fs.ErrNotExist.
const ErrNotDirectory = windows.ERROR_DIRECTORY
