//go:build !windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const envHookConsole = "/dev/tty"

// envHookDocumentsDir is where PowerShell 7 keeps profiles on Linux and macOS is
// elsewhere; this only serves tests and the hook line's Windows layout.
func envHookDocumentsDir(home string) string { return filepath.Join(home, "Documents") }

// envHookRCOwner is the owner an edited startup file keeps. Only a file owned by
// the home's owner is edited, so a root uninstall never rewrites (or takes
// ownership of) a file a symlink in the home points at outside the user's own.
func envHookRCOwner(home string, info fs.FileInfo) (int, int, error) {
	homeInfo, err := os.Stat(home)
	if err != nil {
		return 0, 0, err
	}
	file, ok := info.Sys().(*syscall.Stat_t)
	owner, homeOK := homeInfo.Sys().(*syscall.Stat_t)
	if !ok || !homeOK || file.Uid != owner.Uid {
		return 0, 0, errors.New("not owned by the home directory's owner; left unchanged")
	}
	return int(file.Uid), int(file.Gid), nil
}
