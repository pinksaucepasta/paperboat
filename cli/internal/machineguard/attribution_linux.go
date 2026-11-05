//go:build linux

package machineguard

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// inet_diag wire layout (linux/inet_diag.h); golang.org/x/sys has no types for it.
const (
	inetDiagReqLen   = 56 // inet_diag_req_v2
	inetDiagMsgLen   = 72 // inet_diag_msg
	inetDiagNoCookie = ^uint32(0)
	tcpEstablished   = 1
)

var inetDiagSequence atomic.Uint32

// platformLookupOwners answers which sockets have a tuple: INET_DIAG on Linux.
var platformLookupOwners lookupOwners = lookupOwnersINETDiag

func socketpairCloexec() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}

// lookupOwnersINETDiag asks the kernel for the exact TCP socket with tuple (an INET_DIAG lookup without
// NLM_F_DUMP, a hash lookup) and returns its owner. A socket of another network namespace is never found: the
// netlink socket answers for the guard's own namespace only.
func lookupOwnersINETDiag(tuple socketTuple) ([]socketOwner, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, fmt.Errorf("open sock_diag: %w", err)
	}
	defer unix.Close(fd)
	timeout := unix.NsecToTimeval(int64(attributionBudget))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		return nil, fmt.Errorf("set sock_diag timeout: %w", err)
	}
	sequence := inetDiagSequence.Add(1)
	if err := unix.Sendto(fd, inetDiagRequest(tuple, sequence), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("send sock_diag request: %w", err)
	}
	buffer := make([]byte, 8192)
	var owners []socketOwner
	for deadline := time.Now().Add(attributionBudget); time.Now().Before(deadline); {
		n, _, err := unix.Recvfrom(fd, buffer, 0)
		if err != nil {
			return nil, fmt.Errorf("read sock_diag answer: %w", err)
		}
		messages, err := syscall.ParseNetlinkMessage(buffer[:n])
		if err != nil {
			return nil, fmt.Errorf("parse sock_diag answer: %w", err)
		}
		for _, message := range messages {
			if message.Header.Seq != sequence {
				continue
			}
			switch message.Header.Type {
			case unix.NLMSG_DONE:
				return owners, nil
			case unix.NLMSG_ERROR:
				if len(message.Data) < 4 {
					return nil, errors.New("short sock_diag error")
				}
				code := -int32(binary.NativeEndian.Uint32(message.Data[:4]))
				if code == 0 || syscall.Errno(code) == unix.ENOENT {
					return owners, nil
				}
				return nil, fmt.Errorf("sock_diag lookup: %w", syscall.Errno(code))
			case unix.SOCK_DIAG_BY_FAMILY:
				owner, err := parseInetDiagMessage(message.Data)
				if err != nil {
					return nil, err
				}
				owners = append(owners, owner)
			}
			if message.Header.Flags&unix.NLM_F_MULTI == 0 {
				return owners, nil
			}
		}
	}
	return nil, errors.New("sock_diag answer timed out")
}

func inetDiagRequest(tuple socketTuple, sequence uint32) []byte {
	out := make([]byte, unix.NLMSG_HDRLEN+inetDiagReqLen)
	binary.NativeEndian.PutUint32(out[0:], uint32(len(out)))
	binary.NativeEndian.PutUint16(out[4:], unix.SOCK_DIAG_BY_FAMILY)
	binary.NativeEndian.PutUint16(out[6:], unix.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(out[8:], sequence)
	req := out[unix.NLMSG_HDRLEN:]
	req[0], req[1] = unix.AF_INET, unix.IPPROTO_TCP
	if tuple.Source.Addr().Is6() {
		req[0] = unix.AF_INET6
	}
	binary.NativeEndian.PutUint32(req[4:], ^uint32(0)) // every state; the answer's state is checked
	putSockID(req[8:], tuple)
	return out
}

// putSockID writes inet_diag_sockid: ports and addresses in network order, interface 0, no cookie.
func putSockID(id []byte, tuple socketTuple) {
	binary.BigEndian.PutUint16(id[0:], tuple.Source.Port())
	binary.BigEndian.PutUint16(id[2:], tuple.Destination.Port())
	// Addresses are 16 bytes each; an IPv4 address fills the first 4.
	if tuple.Source.Addr().Is6() {
		source, destination := tuple.Source.Addr().As16(), tuple.Destination.Addr().As16()
		copy(id[4:20], source[:])
		copy(id[20:36], destination[:])
	} else {
		source, destination := tuple.Source.Addr().As4(), tuple.Destination.Addr().As4()
		copy(id[4:8], source[:])
		copy(id[20:24], destination[:])
	}
	binary.NativeEndian.PutUint32(id[40:], inetDiagNoCookie)
	binary.NativeEndian.PutUint32(id[44:], inetDiagNoCookie)
}

func parseInetDiagMessage(data []byte) (socketOwner, error) {
	if len(data) < inetDiagMsgLen || data[0] != unix.AF_INET && data[0] != unix.AF_INET6 {
		return socketOwner{}, errAttributionInconsistent
	}
	id := data[4:52]
	address := func(b []byte) netip.Addr {
		if data[0] == unix.AF_INET6 {
			return netip.AddrFrom16([16]byte(b[:16]))
		}
		return netip.AddrFrom4([4]byte(b[:4]))
	}
	source := netip.AddrPortFrom(address(id[4:20]), binary.BigEndian.Uint16(id[0:]))
	destination := netip.AddrPortFrom(address(id[20:36]), binary.BigEndian.Uint16(id[2:]))
	return socketOwner{
		Tuple:       socketTuple{Source: source, Destination: destination},
		UID:         binary.NativeEndian.Uint32(data[64:]),
		Established: data[1] == tcpEstablished,
		Live:        livePeerState(data[1]),
	}, nil
}

// livePeerState: ESTABLISHED, FIN_WAIT1, FIN_WAIT2, CLOSE_WAIT, LAST_ACK, CLOSING (include/net/tcp_states.h).
func livePeerState(state byte) bool {
	switch state {
	case 1, 4, 5, 8, 9, 11:
		return true
	}
	return false
}
