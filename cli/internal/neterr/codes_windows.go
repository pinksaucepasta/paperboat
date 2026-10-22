//go:build windows

package neterr

import "golang.org/x/sys/windows"

const (
	winsockRefused = windows.WSAECONNREFUSED
	winsockReset   = windows.WSAECONNRESET
	winsockAborted = windows.WSAECONNABORTED
)
