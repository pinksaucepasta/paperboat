//go:build !windows

package bootstrap

import (
	"os"
	"syscall"
)

// insecureEnrollmentTokenFile returns why the file is refused, or "".
func insecureEnrollmentTokenFile(_ string, info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() == 0 || info.Size() > 512 {
		return tokenFileShape
	}
	if info.Mode().Perm() != 0o600 {
		return "only its owner may read and write it (chmod 600)"
	}
	return ""
}
