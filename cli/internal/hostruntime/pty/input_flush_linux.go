package pty

import "golang.org/x/sys/unix"

const ioctlGetTermios = unix.TCGETS

// flushPendingInput discards input queued for the program. TCFLSH on the master flushes the other direction, so it
// opens the slave side through the master (TIOCGPTPEER) and flushes there.
func flushPendingInput(master int) { flushPeer(master, unix.TCIFLUSH) }

// flushPendingOutput discards output the program wrote that nobody read (see the macOS file; Linux never needs it
// for an exit, which does not wait for the output to drain).
func flushPendingOutput(master int) { flushPeer(master, unix.TCOFLUSH) }

func flushPeer(master int, queue int) {
	peer, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(master), unix.TIOCGPTPEER, unix.O_RDONLY|unix.O_NOCTTY|unix.O_CLOEXEC)
	if errno != 0 {
		return
	}
	defer unix.Close(int(peer))
	_ = unix.IoctlSetInt(int(peer), unix.TCFLSH, queue)
}
