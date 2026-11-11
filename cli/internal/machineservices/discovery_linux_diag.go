//go:build linux

package machineservices

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// listenersByDiag lists this user's TCP listeners through NETLINK_SOCK_DIAG, the interface `ss -l` uses. The kernel
// walks only its listening-socket table, so the cost follows the number of listeners; reading /proc/net/tcp renders
// every socket of the machine (about 15 us each, so a machine with 5,000 connections spent 70 ms of CPU every two
// seconds in the worker's port watch, which was most of an idle worker's CPU in the 2026-11-11 pprof).
func listenersByDiag(ctx context.Context, ownerUID uint64) ([]Service, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	timeout := unix.NsecToTimeval(int64(2 * time.Second))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		return nil, err
	}
	var services []Service
	buffer := make([]byte, 64<<10)
	for sequence, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := unix.Sendto(fd, diagRequest(family, uint32(sequence+1)), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
			return nil, err
		}
		found, err := readDiagDump(ctx, fd, buffer, family, ownerUID)
		if err != nil {
			return nil, err
		}
		services = append(services, found...)
	}
	return services, nil
}

const (
	sockDiagByFamily = 20 // SOCK_DIAG_BY_FAMILY, linux/sock_diag.h
	diagListenState   = 10 // TCP_LISTEN, linux/tcp.h
	nlHeaderBytes    = 16
	diagRequestBytes = 56 // struct inet_diag_req_v2
	diagMessageBytes = 72 // struct inet_diag_msg
)

func diagRequest(family uint8, sequence uint32) []byte {
	request := make([]byte, nlHeaderBytes+diagRequestBytes)
	binary.NativeEndian.PutUint32(request[0:], uint32(len(request)))
	binary.NativeEndian.PutUint16(request[4:], sockDiagByFamily)
	binary.NativeEndian.PutUint16(request[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(request[8:], sequence)
	body := request[nlHeaderBytes:]
	body[0], body[1] = family, unix.IPPROTO_TCP
	binary.NativeEndian.PutUint32(body[4:], 1<<diagListenState)
	return request
}

func readDiagDump(ctx context.Context, fd int, buffer []byte, family uint8, ownerUID uint64) ([]Service, error) {
	var services []Service
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, _, err := unix.Recvfrom(fd, buffer, 0)
		if err != nil {
			return nil, err
		}
		for message := buffer[:n]; len(message) >= nlHeaderBytes; {
			length := int(binary.NativeEndian.Uint32(message[0:]))
			if length < nlHeaderBytes || length > len(message) {
				return nil, errors.New("sock_diag: truncated netlink message")
			}
			switch kind := binary.NativeEndian.Uint16(message[4:]); kind {
			case unix.NLMSG_DONE:
				return services, nil
			case unix.NLMSG_ERROR:
				if length >= nlHeaderBytes+4 {
					return nil, fmt.Errorf("sock_diag: %w", unix.Errno(-int32(binary.NativeEndian.Uint32(message[nlHeaderBytes:]))))
				}
				return nil, errors.New("sock_diag: netlink error")
			case sockDiagByFamily:
				if service, ok := diagService(message[nlHeaderBytes:length], family, ownerUID); ok {
					services = append(services, service)
				}
			}
			next := (length + 3) &^ 3 // NLMSG_ALIGN
			if next >= len(message) {
				break
			}
			message = message[next:]
		}
	}
}

// diagService is the one rule parseProcTCP applies to a /proc line: a listener of ownerUID on the wildcard or the
// loopback address, with a port.
func diagService(message []byte, family uint8, ownerUID uint64) (Service, bool) {
	if len(message) < diagMessageBytes || message[0] != family || message[1] != diagListenState {
		return Service{}, false
	}
	port := binary.BigEndian.Uint16(message[4:])
	if port == 0 || uint64(binary.NativeEndian.Uint32(message[64:])) != ownerUID {
		return Service{}, false
	}
	address := message[8:24]
	if family == unix.AF_INET {
		if v4 := [4]byte(address[:4]); v4 == [4]byte{} || v4 == [4]byte{127, 0, 0, 1} {
			return Service{Port: port, Loopback: "127.0.0.1"}, true
		}
		return Service{}, false
	}
	if v6 := [16]byte(address); v6 == [16]byte{} || v6 == [16]byte{15: 1} {
		return Service{Port: port, Loopback: "::1"}, true
	}
	return Service{}, false
}
