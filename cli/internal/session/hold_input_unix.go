//go:build !windows

package session

import (
	"os"

	"golang.org/x/sys/unix"
)

// HoldInput keeps keys typed while pb connects as the raw bytes the terminal sends. A cooked terminal turns Enter (CR)
// into LF before pb reads it, and a Windows target's ConPTY does not take LF as Enter: type-ahead lost every Enter
// (X-24). Like mosh, which puts the terminal in raw mode before it connects (mosh/src/frontend/stmclient.cc
// main_init), only ISIG stays on so Ctrl-C still stops pb while it connects. Run takes over when the shell attaches.
// restore puts the terminal back; stdin that is not a terminal needs nothing.
func HoldInput() (restore func()) {
	fd := int(os.Stdin.Fd())
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return func() {}
	}
	held := *saved
	held.Iflag &^= unix.ICRNL | unix.INLCR | unix.IGNCR | unix.IXON
	held.Lflag &^= unix.ICANON | unix.ECHO
	held.Cc[unix.VMIN], held.Cc[unix.VTIME] = 1, 0
	if unix.IoctlSetTermios(fd, ioctlSetTermios, &held) != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, saved) }
}
