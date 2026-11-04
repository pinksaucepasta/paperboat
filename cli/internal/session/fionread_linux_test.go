//go:build linux

package session

import "golang.org/x/sys/unix"

// fionread asks a pipe how many bytes are unread (FIONREAD).
const fionread = unix.TIOCINQ
