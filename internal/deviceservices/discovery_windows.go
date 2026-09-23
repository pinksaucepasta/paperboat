//go:build windows

package deviceservices

import (
	"context"
	"math/bits"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	tcpTableOwnerPIDAll = 5
	tcpStateListen      = 2
)

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

type tcpRow4 struct {
	State, LocalAddress, LocalPort, RemoteAddress, RemotePort, PID uint32
}

type tcpRow6 struct {
	LocalAddress  [16]byte
	LocalScope    uint32
	LocalPort     uint32
	RemoteAddress [16]byte
	RemoteScope   uint32
	RemotePort    uint32
	State         uint32
	PID           uint32
}

func snapshot(ctx context.Context) ([]Service, error) {
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || currentUser == nil || currentUser.User.Sid == nil {
		return nil, ErrDiscoveryUnavailable
	}
	var services []Service
	for _, family := range []uint32{windows.AF_INET, windows.AF_INET6} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := readTCPTable(family)
		if err != nil {
			return nil, err
		}
		parsed, err := parseTCPTable(data, family, func(pid uint32) bool { return processOwnedBy(pid, currentUser.User.Sid) })
		if err != nil {
			return nil, err
		}
		services = append(services, parsed...)
	}
	return services, ctx.Err()
}

func readTCPTable(family uint32) ([]byte, error) {
	var size uint32
	var data []byte
	for attempt := 0; attempt < 3; attempt++ {
		var address uintptr
		if len(data) != 0 {
			address = uintptr(unsafe.Pointer(&data[0]))
		}
		result, _, _ := getExtendedTCPTable.Call(address, uintptr(unsafe.Pointer(&size)), 1, uintptr(family), tcpTableOwnerPIDAll, 0)
		if result == 0 {
			if size > uint32(len(data)) {
				return nil, ErrDiscoveryUnavailable
			}
			return data[:size], nil
		}
		if windows.Errno(result) != windows.ERROR_INSUFFICIENT_BUFFER || size < 4 || size > maximumSnapshotBytes {
			if size > maximumSnapshotBytes {
				return nil, ErrSnapshotTooLarge
			}
			return nil, windows.Errno(result)
		}
		data = make([]byte, size)
	}
	return nil, ErrDiscoveryUnavailable
}

func parseTCPTable(data []byte, family uint32, owned func(uint32) bool) ([]Service, error) {
	if len(data) < 4 {
		return nil, ErrDiscoveryUnavailable
	}
	count := *(*uint32)(unsafe.Pointer(&data[0]))
	rowSize := int(unsafe.Sizeof(tcpRow4{}))
	if family == windows.AF_INET6 {
		rowSize = int(unsafe.Sizeof(tcpRow6{}))
	}
	if uint64(count) > uint64((len(data)-4)/rowSize) {
		return nil, ErrDiscoveryUnavailable
	}
	services := make([]Service, 0)
	for index := uint32(0); index < count; index++ {
		row := unsafe.Pointer(&data[4+int(index)*rowSize])
		if family == windows.AF_INET {
			value := (*tcpRow4)(row)
			if value.State != tcpStateListen || !owned(value.PID) {
				continue
			}
			address := bits.ReverseBytes32(value.LocalAddress)
			if address == 0 || address == 0x7f000001 {
				services = append(services, Service{Port: windowsPort(value.LocalPort), Loopback: "127.0.0.1"})
			}
			continue
		}
		value := (*tcpRow6)(row)
		if value.State != tcpStateListen || !owned(value.PID) {
			continue
		}
		address := netip.AddrFrom16(value.LocalAddress).Unmap()
		if address.IsUnspecified() || address == netip.IPv6Loopback() {
			services = append(services, Service{Port: windowsPort(value.LocalPort), Loopback: "::1"})
		}
	}
	return services, nil
}

func processOwnedBy(pid uint32, expected *windows.SID) bool {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	return err == nil && user != nil && user.User.Sid != nil && user.User.Sid.Equals(expected)
}

func windowsPort(value uint32) uint16 { return uint16(bits.ReverseBytes32(value) >> 16) }
