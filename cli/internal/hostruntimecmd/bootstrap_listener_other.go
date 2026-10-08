//go:build !windows

package hostruntimecmd

import (
	"errors"
	"syscall"
)

func bootstrapAddressInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
