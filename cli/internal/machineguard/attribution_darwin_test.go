//go:build darwin

package machineguard

import (
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestPCBListAttributesALiveLoopbackConnectionToItsUID runs against the real kernel: the connecting socket of a
// loopback connection this test opened is owned by the test's uid and established.
func TestPCBListAttributesALiveLoopbackConnectionToItsUID(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	tuple, err := clientTuple(accepted)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := attributeOwner(tuple, lookupOwnersPCBList)
	if err != nil || uid != uint32(os.Getuid()) {
		t.Fatalf("attributed uid=%d err=%v; want %d", uid, err, os.Getuid())
	}
}

// TestPCBListAttributionAt10000Sockets (opt-in: PB_PCB_SCALE=1, needs a raised fd limit) holds 5,000 loopback
// connections (10,000 sockets) and reports the time of one attribution; the F23-01 budget is 2 ms.
func TestPCBListAttributionAt10000Sockets(t *testing.T) {
	if os.Getenv("PB_PCB_SCALE") != "1" {
		t.Skip("set PB_PCB_SCALE=1 to hold 10,000 sockets")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 5000; i++ {
		c, err := net.Dial("tcp4", listener.Addr().String())
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		a, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c, a)
	}
	tuple, err := clientTuple(conns[len(conns)-1])
	if err != nil {
		t.Fatal(err)
	}
	var worst time.Duration
	var all, copies []time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		_, _ = unix.SysctlRaw("net.inet.tcp.pcblist_n")
		copies = append(copies, time.Since(start))
	}
	for i := 0; i < 20; i++ {
		start := time.Now()
		owners, err := lookupOwnersPCBList(tuple)
		elapsed := time.Since(start)
		all = append(all, elapsed)
		if elapsed > worst {
			worst = elapsed
		}
		if err != nil || len(owners) != 1 || owners[0].UID != uint32(os.Getuid()) {
			t.Fatalf("owners=%+v err=%v", owners, err)
		}
	}
	slices.Sort(all)
	slices.Sort(copies)
	t.Logf("at 10,000 sockets: attribution median %v worst %v; sysctl copy alone median %v worst %v", all[len(all)/2], worst, copies[len(copies)/2], copies[len(copies)-1])
	if worst > 2*time.Millisecond {
		t.Fatalf("attribution took %v; the budget is 2 ms", worst)
	}
}
