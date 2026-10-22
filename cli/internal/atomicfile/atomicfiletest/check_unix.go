//go:build !windows

package atomicfiletest

import (
	"fmt"
	"os"
)

func checkOwnerOnly(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != mode {
		return fmt.Errorf("mode %#o, want %#o", info.Mode().Perm(), mode)
	}
	return nil
}
