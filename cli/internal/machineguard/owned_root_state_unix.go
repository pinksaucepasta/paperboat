//go:build linux || darwin

package machineguard

import (
	"errors"
	"os"
	"syscall"
)

func validateOwnedRootStateFile(_ string, name string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("unsafe machine guard certificate state ownership; preserved")
	}
	if name == "rootCA-key.pem" && info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe machine guard private key permissions; preserved")
	}
	return nil
}
