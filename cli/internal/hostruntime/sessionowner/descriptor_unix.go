//go:build linux || darwin

package sessionowner

import (
	"os"
	"syscall"
)

func validateDescriptorCustody(_ string, info os.FileInfo, _ Descriptor) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return ErrInvalid
	}
	return nil
}
