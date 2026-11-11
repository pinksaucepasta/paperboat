//go:build unix

package filetransfer

import (
	"os"
	"syscall"
)

func fileLinks(path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return int(info.Sys().(*syscall.Stat_t).Nlink), nil
}
