package pty

import "golang.org/x/sys/unix"

const ioctlGetTermios = unix.TIOCGETA

// shortcut: the queued paste is not discarded on macOS (no TIOCGPTPEER), upgrade when macOS terminal rows start.
func flushPendingInput(int) {}
