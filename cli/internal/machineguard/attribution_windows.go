//go:build windows

package machineguard

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows attribution (F23-01, F23-02). The kernel's TCP table names the process that bound each connection and
// when (GetExtendedTcpTable, read as tailscale net/netstat/netstat_windows.go does); the OS user is that
// process's token user. A pid alone is not an identity: Windows gives it to a new process once the old one is
// gone, while a socket another process inherited keeps naming it. So a process created after the bind is refused
// (the creation-time check of tailscale util/winutil/restartmgr_windows.go).

const (
	tcpTableOwnerModuleConnections = 7        // TCP_TABLE_OWNER_MODULE_CONNECTIONS
	tcpTableHeaderLen              = 8        // dwNumEntries, padded to the rows' alignment
	tcpRowOwnerModuleLen           = 160      // MIB_TCPROW_OWNER_MODULE
	tcp6RowOwnerModuleLen          = 192      // MIB_TCP6ROW_OWNER_MODULE
	tcpStateEstablished            = 5        // MIB_TCP_STATE_ESTAB
	tcpStateCloseWait              = 8        // MIB_TCP_STATE_CLOSE_WAIT; 6 and 7 are FIN_WAIT1 and FIN_WAIT2
	maxTCPTableLen                 = 64 << 20 // about 400,000 connections
)

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// tcpOwner is one kernel answer for a tuple: UID is the pid of the process that bound the connection, bound is
// when it did (FILETIME ticks).
type tcpOwner struct {
	socketOwner
	bound int64
}

// connectionOwnerSID is the SID of the OS user that owns the client end of an accepted loopback TCP connection.
// It fails closed: no single live owner, or a process that cannot be shown to be the one that bound it, is an error.
func connectionOwnerSID(accepted net.Conn) (string, error) {
	return attributeSID(accepted, lookupTCPOwners, processUserSID, false)
}

func attributeSID(accepted net.Conn, lookup func(socketTuple) ([]tcpOwner, error), user func(pid uint32, bound int64) (string, error), live bool) (string, error) {
	tuple, err := clientTuple(accepted)
	if err != nil {
		return "", err
	}
	var bound int64 // bind time of the row of the last answer; attributeOwner accepts only that answer
	pid, err := attributeOwnerState(tuple, func(tuple socketTuple) ([]socketOwner, error) {
		rows, err := lookup(tuple)
		owners := make([]socketOwner, len(rows))
		bound = 0
		for i, row := range rows {
			owners[i] = row.socketOwner
			bound = row.bound
		}
		return owners, err
	}, live)
	if err != nil {
		return "", err
	}
	return user(pid, bound)
}

func lookupTCPOwners(tuple socketTuple) ([]tcpOwner, error) {
	family := uint32(windows.AF_INET)
	if tuple.Source.Addr().Is6() && tuple.Destination.Addr().Is6() {
		family = windows.AF_INET6
	} else if !tuple.Source.Addr().Is4() || !tuple.Destination.Addr().Is4() {
		return nil, errAttributionUnsupported
	}
	size := uint32(64 << 10)
	for range 4 {
		table := make([]byte, size)
		result, _, _ := getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&table[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(family), tcpTableOwnerModuleConnections, 0)
		switch windows.Errno(result) {
		case windows.ERROR_SUCCESS:
			return matchTCPOwners(table, tuple)
		case windows.ERROR_INSUFFICIENT_BUFFER:
			if size > maxTCPTableLen {
				return nil, fmt.Errorf("the TCP connection table needs %d bytes", size)
			}
			size += size / 4 // connections opened before the next call still fit
		default:
			return nil, fmt.Errorf("read the TCP connection table: %w", windows.Errno(result))
		}
	}
	return nil, errors.New("the TCP connection table kept growing")
}

// matchTCPOwners returns the rows of a MIB_TCPTABLE_OWNER_MODULE whose connecting socket is tuple.
func matchTCPOwners(table []byte, tuple socketTuple) ([]tcpOwner, error) {
	if len(table) < tcpTableHeaderLen {
		return nil, errors.New("short TCP connection table")
	}
	rows := table[tcpTableHeaderLen:]
	count := binary.LittleEndian.Uint32(table)
	// Row layouts (tcpmib.h), little-endian words, addresses and ports in network byte order. IPv4: state, local
	// address, local port, remote address, remote port, pid, bind time. IPv6: local address, scope, port, remote
	// address, scope, port, state, pid, bind time.
	length, v6 := tcpRowOwnerModuleLen, tuple.Source.Addr().Is6()
	if v6 {
		length = tcp6RowOwnerModuleLen
	}
	if uint64(count)*uint64(length) > uint64(len(rows)) {
		return nil, errors.New("truncated TCP connection table")
	}
	var owners []tcpOwner
	for ; count > 0; count, rows = count-1, rows[length:] {
		var local, remote netip.AddrPort
		var state, pid uint32
		var bound int64
		if v6 {
			local = netip.AddrPortFrom(netip.AddrFrom16([16]byte(rows[0:16])), binary.BigEndian.Uint16(rows[20:22]))
			remote = netip.AddrPortFrom(netip.AddrFrom16([16]byte(rows[24:40])), binary.BigEndian.Uint16(rows[44:46]))
			state, pid, bound = binary.LittleEndian.Uint32(rows[48:]), binary.LittleEndian.Uint32(rows[52:]), int64(binary.LittleEndian.Uint64(rows[56:]))
		} else {
			local = netip.AddrPortFrom(netip.AddrFrom4([4]byte(rows[4:8])), binary.BigEndian.Uint16(rows[8:10]))
			remote = netip.AddrPortFrom(netip.AddrFrom4([4]byte(rows[12:16])), binary.BigEndian.Uint16(rows[16:18]))
			state, pid, bound = binary.LittleEndian.Uint32(rows), binary.LittleEndian.Uint32(rows[20:]), int64(binary.LittleEndian.Uint64(rows[24:]))
		}
		if local != tuple.Source || remote != tuple.Destination {
			continue
		}
		owners = append(owners, tcpOwner{bound: bound, socketOwner: socketOwner{
			Tuple: tuple, UID: pid,
			Established: state == tcpStateEstablished, Live: state >= tcpStateEstablished && state <= tcpStateCloseWait,
		}})
	}
	return owners, nil
}

// processUserSID is the user of process pid, which must have existed when the connection was bound.
func processUserSID(pid uint32, bound int64) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", errors.Join(errAttributionNotFound, fmt.Errorf("open the connecting process: %w", err))
	}
	defer windows.CloseHandle(process)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user); err != nil {
		return "", errors.Join(errAttributionNotFound, fmt.Errorf("read the connecting process's start time: %w", err))
	}
	if !processBoundConnection(int64(created.HighDateTime)<<32|int64(created.LowDateTime), bound) {
		return "", errAttributionInconsistent
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return "", errors.Join(errAttributionNotFound, fmt.Errorf("open the connecting process's token: %w", err))
	}
	defer token.Close()
	owner, err := token.GetTokenUser()
	if err != nil || owner.User.Sid == nil || !owner.User.Sid.IsValid() {
		return "", errors.Join(errAttributionNotFound, errors.New("the connecting process has no user"), err)
	}
	return owner.User.Sid.String(), nil
}

// processBoundConnection reports whether a process created at created can be the one that bound a connection at
// bound: the process still holds the pid (its handle is open), so it is that process unless it started later.
func processBoundConnection(created, bound int64) bool {
	return bound != 0 && created <= bound
}

// ConnectionOwnerSID is connectionOwnerSID for the per-user preview proxy, which serves a client that may already
// have sent its request and half-closed (FIN_WAIT2 still names its user), like Linux's ConnectionOwnerUID.
func ConnectionOwnerSID(accepted net.Conn) (string, error) {
	return attributeSID(accepted, lookupTCPOwners, processUserSID, true)
}
