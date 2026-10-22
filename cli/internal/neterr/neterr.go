// Package neterr recognises connection failures on every platform. Winsock reports its own error
// numbers (WSAECONNREFUSED is 10061); the syscall.ECONNREFUSED value Windows also defines is a
// different, invented number, so errors.Is(err, syscall.ECONNREFUSED) is false for a connection that
// was really refused on Windows.
package neterr

import (
	"errors"
	"syscall"
)

// Refused reports whether err says nothing was listening at the address dialed.
func Refused(err error) bool { return matches(err, syscall.ECONNREFUSED, winsockRefused) }

// Reset reports whether err says the peer reset the connection.
func Reset(err error) bool { return matches(err, syscall.ECONNRESET, winsockReset) }

// Aborted reports whether err says the connection was aborted locally or by the network stack.
func Aborted(err error) bool { return matches(err, syscall.ECONNABORTED, winsockAborted) }

func matches(err error, portable, winsock syscall.Errno) bool {
	return errors.Is(err, portable) || (winsock != 0 && errors.Is(err, winsock))
}
