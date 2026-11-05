//go:build darwin

package machineguard

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// platformLookupOwners answers which sockets have a tuple: the TCP pcb list netstat reads on macOS.
var platformLookupOwners lookupOwners = lookupOwnersPCBList

func lookupOwnersPCBList(tuple socketTuple) ([]socketOwner, error) {
	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil {
		return nil, err
	}
	return parsePCBList(buf, tuple)
}

// socketpairCloexec makes the route pair; macOS has no SOCK_CLOEXEC, so close-on-exec is set under ForkLock.
func socketpairCloexec() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return pair, err
	}
	unix.CloseOnExec(pair[0])
	unix.CloseOnExec(pair[1])
	return pair, nil
}
