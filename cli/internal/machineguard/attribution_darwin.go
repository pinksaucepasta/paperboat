//go:build darwin

package machineguard

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// platformLookupOwners answers which sockets have a tuple: the TCP pcb list netstat reads on macOS.
var platformLookupOwners lookupOwners = lookupOwnersPCBList

// pcbSnapshots shares one in-flight pcb-list copy among concurrent lookups. macOS budget (F23-01): the copy is
// linear in the host's sockets, ~11 ms at 10,000 (no per-connection lookup exists, netstat and lsof scan the same
// list), paid once per new gateway connection: ≤15 ms worst at 10,000 sockets and ≤2 ms at 1,000.
var pcbSnapshots = &snapshotFlight{fetch: func() ([]byte, error) { return unix.SysctlRaw("net.inet.tcp.pcblist_n") }}

func lookupOwnersPCBList(tuple socketTuple) ([]socketOwner, error) {
	buf, err := pcbSnapshots.get()
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
