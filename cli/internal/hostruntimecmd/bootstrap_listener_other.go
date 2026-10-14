//go:build !windows

package hostruntimecmd

import (
	"syscall"
)

func bootstrapAddressInUse(err error) bool { return runtimeCausesOnly(err, syscall.EADDRINUSE) }
