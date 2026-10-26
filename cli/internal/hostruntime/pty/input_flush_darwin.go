package pty

import (
	"bytes"
	"unsafe"

	"golang.org/x/sys/unix"
)

const ioctlGetTermios = unix.TIOCGETA

// flushPendingInput discards input queued for the program. Flushing on the master clears the other direction, so
// it opens the slave by name (TIOCPTYGNAME; macOS has no TIOCGPTPEER) without making it a controlling terminal and
// flushes its read side there.
func flushPendingInput(master int) {
	var name [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(master), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		return
	}
	end := bytes.IndexByte(name[:], 0)
	if end <= 0 {
		return
	}
	peer, err := unix.Open(string(name[:end]), unix.O_RDONLY|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer unix.Close(peer)
	read := int32(0x0001) // FREAD (<sys/fcntl.h>): flush the input queue only
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, uintptr(peer), unix.TIOCFLUSH, uintptr(unsafe.Pointer(&read)))
}
