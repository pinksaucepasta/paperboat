//go:build darwin || linux

package runtime

import (
	"os"
	"syscall"
)

// cacheFileTrusted: the remembered-address file is read only when this account owns it and nobody else can write or
// read it (it is written 0600 by remember), so a file another account dropped into the state directory pins nothing.
func cacheFileTrusted(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid() && info.Mode().Perm()&0o077 == 0
}
