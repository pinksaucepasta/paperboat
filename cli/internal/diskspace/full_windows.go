package diskspace

import (
	"errors"
	"syscall"
)

func fullPlatform(err error) bool {
	return errors.Is(err, syscall.Errno(39)) || errors.Is(err, syscall.Errno(112))
}
