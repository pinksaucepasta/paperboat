//go:build windows

package server

import (
	"errors"

	"golang.org/x/sys/windows"
)

// writeProtected: Windows has no EROFS; a read-only volume (an NTFS disk attached read-only, a write-protected
// medium) refuses writes with ERROR_WRITE_PROTECT, which is as permanent as a read-only file system elsewhere.
func writeProtected(err error) bool { return errors.Is(err, windows.ERROR_WRITE_PROTECT) }
