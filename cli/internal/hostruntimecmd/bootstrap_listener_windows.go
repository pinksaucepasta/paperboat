//go:build windows

package hostruntimecmd

import (
	"errors"
	"golang.org/x/sys/windows"
)

func bootstrapAddressInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }
