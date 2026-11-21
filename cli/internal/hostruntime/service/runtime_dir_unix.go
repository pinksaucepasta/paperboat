//go:build !windows

package service

import (
	"os"
	"syscall"
)

// ownRuntimeDir is true for a real directory (not a link) owned by uid, as pb config's query requires.
func ownRuntimeDir(path string, uid int) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(owner.Uid) == uid
}
