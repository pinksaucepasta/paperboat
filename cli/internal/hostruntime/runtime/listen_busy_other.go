//go:build !windows

package runtime

import (
	"errors"
	"syscall"
)

func addressBusy(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
