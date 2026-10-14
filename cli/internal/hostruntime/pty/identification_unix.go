//go:build darwin || linux

package pty

import "golang.org/x/sys/unix"

// Identification queries the foreground process group, rather than assuming
// that the shell originally launched by the adapter still owns the terminal.
func (p *Process) Identification() Identification {
	select {
	case <-p.done:
		return Identification{}
	default:
	}
	connection, err := p.file.SyscallConn()
	if err != nil {
		return Identification{}
	}
	var result Identification
	// Control holds the file descriptor open during the query, including when
	// CloseIO runs concurrently. Never query a descriptor returned by File.Fd:
	// closing and reusing that descriptor could identify an unrelated terminal.
	if err := connection.Control(func(fd uintptr) {
		group, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
		if err != nil || group <= 0 {
			return
		}
		snapshot := identifyForeground(group)
		// Discard a snapshot taken across a foreground-job transition.
		current, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
		if err == nil && current == group {
			result = snapshot
		}
	}); err != nil {
		return Identification{}
	}
	return result
}
