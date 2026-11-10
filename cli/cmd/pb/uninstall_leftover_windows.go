//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// leftoverLockedBridgeDirectory reports a removal failure that is only an empty elevation request directory another
// process (winget's COM server, started by the OpenSSH preparation, was seen holding one open) keeps open: Windows
// cannot delete a directory with an open handle, nothing is inside it and a retry fails the same way, so it must
// not make a reset or uninstall fail. Anything else, also a locked directory with content, stays an error.
func leftoverLockedBridgeDirectory(err error) bool {
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || !errors.Is(pathErr.Err, windows.ERROR_SHARING_VIOLATION) {
		return false
	}
	directory := filepath.Clean(pathErr.Path)
	if !strings.HasPrefix(filepath.Base(directory), "request-") || !strings.EqualFold(filepath.Base(filepath.Dir(directory)), "elevation") {
		return false
	}
	entries, readErr := os.ReadDir(directory)
	return readErr == nil && len(entries) == 0
}
