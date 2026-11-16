//go:build windows

package machineservices

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestParseTCPTableFiltersProcessOwner(t *testing.T) {
	rowSize := int(unsafe.Sizeof(tcpRow4{}))
	data := make([]byte, 4+3*rowSize)
	binary.LittleEndian.PutUint32(data, 3)
	rows := (*[3]tcpRow4)(unsafe.Pointer(&data[4]))
	rows[0] = tcpRow4{State: tcpStateListen, LocalAddress: 0, LocalPort: 0x00003815, PID: 10}
	rows[1] = tcpRow4{State: tcpStateListen, LocalAddress: 0x0100007f, LocalPort: 0x00003915, PID: 11}
	rows[2] = tcpRow4{State: tcpStateListen, LocalAddress: 0x0200007f, LocalPort: 0x00003a15, PID: 10}
	got, err := parseTCPTable(data, windows.AF_INET, func(pid uint32) bool { return pid == 10 })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (Service{Port: 5432, Loopback: "127.0.0.1"}) {
		t.Fatalf("services=%#v", got)
	}
}

func TestParseTCPTableListsWebPortsOfAnyProcess(t *testing.T) {
	rowSize := int(unsafe.Sizeof(tcpRow4{}))
	data := make([]byte, 4+3*rowSize)
	binary.LittleEndian.PutUint32(data, 3)
	rows := (*[3]tcpRow4)(unsafe.Pointer(&data[4]))
	rows[0] = tcpRow4{State: tcpStateListen, LocalAddress: 0, LocalPort: 0x00005000, PID: 4}
	rows[1] = tcpRow4{State: tcpStateListen, LocalAddress: 0, LocalPort: 0x0000bb01, PID: 4}
	rows[2] = tcpRow4{State: tcpStateListen, LocalAddress: 0, LocalPort: 0x00001600, PID: 4}
	got, err := parseTCPTable(data, windows.AF_INET, func(uint32) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (Service{Port: 80, Loopback: "127.0.0.1"}) || got[1] != (Service{Port: 443, Loopback: "127.0.0.1"}) {
		t.Fatalf("the system's 80 and 443 are listed, its 22 is not: %#v", got)
	}
}
