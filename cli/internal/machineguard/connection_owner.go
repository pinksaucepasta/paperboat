//go:build linux || darwin

package machineguard

import "net"

// ConnectionOwnerUID is the OS user that owns the client end of an accepted loopback TCP connection, found from
// the kernel by the exact tuple (the guard's gateway attribution: INET_DIAG on Linux, pcblist_n on macOS). It
// fails closed: no single live owner is an error.
func ConnectionOwnerUID(accepted net.Conn) (uint32, error) {
	tuple, err := clientTuple(accepted)
	if err != nil {
		return 0, err
	}
	return attributeLiveOwner(tuple, platformLookupOwners)
}
