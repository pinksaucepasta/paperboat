//go:build windows

package windowsopenssh

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Match the existing managedssh TCP owner-query allocation bound. Tables can
// change during enumeration, so retries and the number of matching listeners
// are also bounded. Decode rows explicitly rather than casting unchecked data.
const nativeHealthMaximumTCPTable = 10 << 20
const nativeHealthMaximumListeners = 1024

var nativeHealthTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

func collectNativeLoopbackListeners(port uint16) ([]ListenerRecord, error) {
	if port == 0 {
		return nil, ErrInvalidConfig
	}
	var listeners []ListenerRecord
	for _, family := range []uint32{windows.AF_INET, windows.AF_INET6} {
		table, err := nativeListenerTable(family)
		if err != nil {
			return nil, fmt.Errorf("%w: TCP listener table: %v", ErrServiceUnhealthy, err)
		}
		rows, err := decodeNativeListenerTable(table, family, port)
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, rows...)
		if len(listeners) > nativeHealthMaximumListeners {
			return nil, ErrServiceUnhealthy
		}
	}
	// Pin each matching process while obtaining its image and parent identity.
	// Query-only handles do not grant termination, token or memory access.
	handles := make(map[uint32]windows.Handle)
	defer func() {
		for _, handle := range handles {
			windows.CloseHandle(handle)
		}
	}()
	images := make(map[uint32]string)
	for _, listener := range listeners {
		if _, ok := handles[listener.ProcessID]; ok {
			continue
		}
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, listener.ProcessID)
		if err != nil {
			return nil, fmt.Errorf("%w: listener process query: %v", ErrServiceUnhealthy, err)
		}
		handles[listener.ProcessID] = handle
		buffer := make([]uint16, 32768)
		size := uint32(len(buffer))
		if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil || size == 0 || size > uint32(len(buffer)) {
			return nil, fmt.Errorf("%w: listener image query: %v", ErrServiceUnhealthy, err)
		}
		images[listener.ProcessID] = windows.UTF16ToString(buffer[:size])
	}
	if len(listeners) == 0 {
		return listeners, nil
	}
	parents, err := nativeListenerParents(handles)
	if err != nil {
		return nil, err
	}
	for index := range listeners {
		row := &listeners[index]
		parent, ok := parents[row.ProcessID]
		if !ok {
			return nil, fmt.Errorf("%w: listener process disappeared", ErrServiceUnhealthy)
		}
		var exitCode uint32
		if err := windows.GetExitCodeProcess(handles[row.ProcessID], &exitCode); err != nil || exitCode != 259 {
			return nil, fmt.Errorf("%w: listener process is not running", ErrServiceUnhealthy)
		}
		row.ParentProcessID = parent
		row.ExecutablePath = images[row.ProcessID]
	}
	return listeners, nil
}

func nativeListenerTable(family uint32) ([]byte, error) {
	var size uint32
	var buffer []byte
	for attempt := 0; attempt < 8; attempt++ {
		var pointer uintptr
		if len(buffer) > 0 {
			pointer = uintptr(unsafe.Pointer(&buffer[0]))
		}
		// TCP_TABLE_OWNER_PID_LISTENER=3; both v4 and v6 return owner-PID rows.
		result, _, _ := nativeHealthTCPTable.Call(pointer, uintptr(unsafe.Pointer(&size)), 0, uintptr(family), 3, 0)
		runtime.KeepAlive(buffer)
		if result == 0 {
			if size < 4 || size > uint32(len(buffer)) {
				return nil, ErrServiceUnhealthy
			}
			return buffer[:size], nil
		}
		if result != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, windows.Errno(result)
		}
		if size < 4 || size > nativeHealthMaximumTCPTable {
			return nil, ErrServiceUnhealthy
		}
		buffer = make([]byte, int(size))
	}
	return nil, fmt.Errorf("%w: TCP listener table changed repeatedly", ErrServiceUnhealthy)
}

func decodeNativeListenerTable(table []byte, family uint32, port uint16) ([]ListenerRecord, error) {
	if len(table) < 4 || len(table) > nativeHealthMaximumTCPTable {
		return nil, ErrServiceUnhealthy
	}
	rowSize, portOffset, stateOffset, pidOffset := 24, 8, 0, 20
	if family == windows.AF_INET6 {
		rowSize, portOffset, stateOffset, pidOffset = 56, 20, 48, 52
	} else if family != windows.AF_INET {
		return nil, ErrInvalidConfig
	}
	count := binary.LittleEndian.Uint32(table[:4])
	if uint64(count) > uint64((len(table)-4)/rowSize) {
		return nil, ErrServiceUnhealthy
	}
	var listeners []ListenerRecord
	for index := uint32(0); index < count; index++ {
		row := table[4+int(index)*rowSize : 4+(int(index)+1)*rowSize]
		if binary.BigEndian.Uint16(row[portOffset:portOffset+2]) != port {
			continue
		}
		if binary.LittleEndian.Uint32(row[stateOffset:stateOffset+4]) != 2 {
			return nil, ErrServiceUnhealthy
		}
		pid := binary.LittleEndian.Uint32(row[pidOffset : pidOffset+4])
		if pid == 0 {
			return nil, ErrServiceUnhealthy
		}
		var address string
		if family == windows.AF_INET {
			address = net.IP(row[4:8]).String()
		} else {
			address = net.IP(row[:16]).String()
		}
		listeners = append(listeners, ListenerRecord{Address: address, Port: port, ProcessID: pid})
		if len(listeners) > nativeHealthMaximumListeners {
			return nil, ErrServiceUnhealthy
		}
	}
	return listeners, nil
}

func nativeListenerParents(wanted map[uint32]windows.Handle) (map[uint32]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: process snapshot: %v", ErrServiceUnhealthy, err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, fmt.Errorf("%w: process snapshot entry: %v", ErrServiceUnhealthy, err)
	}
	parents := make(map[uint32]uint32, len(wanted))
	for count := 0; count < 65536; count++ {
		if _, ok := wanted[entry.ProcessID]; ok {
			parents[entry.ProcessID] = entry.ParentProcessID
		}
		err := windows.Process32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return parents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: process snapshot entry: %v", ErrServiceUnhealthy, err)
		}
	}
	return nil, fmt.Errorf("%w: process snapshot exceeds bound", ErrServiceUnhealthy)
}
