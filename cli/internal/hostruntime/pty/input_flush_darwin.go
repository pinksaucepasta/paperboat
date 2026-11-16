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
func flushPendingInput(master int) { flushPeer(master, 0x0001) } // FREAD (<sys/fcntl.h>): the input queue only

// flushPendingOutput discards output the program wrote that nobody read: on macOS a session leader's exit waits
// until its terminal output drains, so a shell that printed while exiting stays unreaped (see Terminate).
func flushPendingOutput(master int) { flushPeer(master, 0x0002) } // FWRITE: the output queue only

func flushPeer(master int, queue int32) {
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
	_, _, _ = unix.Syscall(unix.SYS_IOCTL, uintptr(peer), unix.TIOCFLUSH, uintptr(unsafe.Pointer(&queue)))
}
