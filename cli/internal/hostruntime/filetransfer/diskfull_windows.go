package filetransfer

import (
	"errors"
	"syscall"
)

// ERROR_HANDLE_DISK_FULL (39) and ERROR_DISK_FULL (112) are how Windows reports a full disk.
func diskFullPlatform(err error) bool {
	return errors.Is(err, syscall.Errno(39)) || errors.Is(err, syscall.Errno(112))
}
