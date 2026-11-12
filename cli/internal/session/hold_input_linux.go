package session

import "golang.org/x/sys/unix"

const ioctlGetTermios, ioctlSetTermios = unix.TCGETS, unix.TCSETS
