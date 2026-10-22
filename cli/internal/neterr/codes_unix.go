//go:build !windows

package neterr

import "syscall"

// Only the portable values exist outside Windows.
const (
	winsockRefused syscall.Errno = 0
	winsockReset   syscall.Errno = 0
	winsockAborted syscall.Errno = 0
)
