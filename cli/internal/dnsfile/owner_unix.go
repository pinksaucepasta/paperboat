//go:build darwin || linux

package dnsfile

import (
	"os"
	"syscall"
)

// trusted: the address file is read only when this account owns it and nobody else can write or
// read it (it is written 0600 by Open), so a file another account dropped into the state directory pins nothing.
func trusted(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid() && info.Mode().Perm()&0o077 == 0
}
