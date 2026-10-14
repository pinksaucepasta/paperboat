//go:build windows

package hostruntimecmd

import (
	"golang.org/x/sys/windows"
)

func bootstrapAddressInUse(err error) bool { return runtimeCausesOnly(err, windows.WSAEADDRINUSE) }
