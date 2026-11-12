package session

import "golang.org/x/sys/unix"

const ioctlGetTermios, ioctlSetTermios = unix.TIOCGETA, unix.TIOCSETA
