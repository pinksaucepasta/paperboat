//go:build unix

package bugreport

import "os"

func checkBundleFile(_ string, info os.FileInfo) (bool, error) {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0, nil
}
