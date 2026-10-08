//go:build !windows

package enrollment

import (
	"os"
	"syscall"
)

func checkEnrollmentConfigFile(_ string, info os.FileInfo, maximum int64) (bool, error) {
	if info == nil {
		return false, nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode().Perm() == 0o600 && stat.Nlink == 1 && info.Size() <= maximum, nil
}

func secureEnrollmentConfigFile(path string, info os.FileInfo, maximum int64) bool {
	valid, err := checkEnrollmentConfigFile(path, info, maximum)
	return valid && err == nil
}
