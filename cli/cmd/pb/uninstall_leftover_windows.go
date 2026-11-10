//go:build windows

package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// lockedBridgeLeftover reports an elevation request directory that a removal could not delete only because another
// process (winget's COM server, started by the OpenSSH preparation, was seen holding one open) keeps a handle on it:
// Windows cannot delete a directory with an open handle, nothing is inside it and a retry fails the same way. The
// caller still reports the path; nothing is deleted or hidden here.
//
// Nothing is judged by path after the failure. The parent (named elevation) is opened once as a root and checked on
// that handle, the child is opened relative to it (a link that leaves the root is refused), and the child's handle
// shows that it is no reparse point, belongs to the user, Administrators or LocalSystem
// (checkWindowsStateDirectory) and has no entry. A directory that is not provably empty, not provably ours or a link
// is not a leftover and stays the failure it was.
func lockedBridgeLeftover(err error) (string, bool) {
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || !errors.Is(pathErr.Err, windows.ERROR_SHARING_VIOLATION) {
		return "", false
	}
	directory := filepath.Clean(pathErr.Path)
	parentPath, name := filepath.Dir(directory), filepath.Base(directory)
	if !strings.HasPrefix(name, "request-") || !strings.EqualFold(filepath.Base(parentPath), "elevation") {
		return "", false
	}
	parent, openErr := os.OpenRoot(parentPath)
	if openErr != nil {
		return "", false
	}
	defer parent.Close()
	if checkWindowsStateDirectory(parent, parentPath) != nil {
		return "", false
	}
	child, openErr := parent.OpenRoot(name)
	if openErr != nil {
		return "", false
	}
	defer child.Close()
	if checkWindowsStateDirectory(child, directory) != nil {
		return "", false
	}
	self, openErr := child.Open(".")
	if openErr != nil {
		return "", false
	}
	defer self.Close()
	if _, readErr := self.Readdirnames(1); !errors.Is(readErr, io.EOF) {
		return "", false
	}
	return directory, true
}
