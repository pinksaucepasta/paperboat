//go:build windows

package runtime

import (
	"errors"

	"golang.org/x/sys/windows"
)

func addressBusy(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}
