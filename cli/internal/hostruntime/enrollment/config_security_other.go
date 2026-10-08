//go:build !windows && !darwin && !linux

package enrollment

import "os"

func checkEnrollmentConfigFile(_ string, info os.FileInfo, maximum int64) (bool, error) {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0 && info.Size() <= maximum, nil
}

func secureEnrollmentConfigFile(path string, info os.FileInfo, maximum int64) bool {
	valid, err := checkEnrollmentConfigFile(path, info, maximum)
	return valid && err == nil
}
