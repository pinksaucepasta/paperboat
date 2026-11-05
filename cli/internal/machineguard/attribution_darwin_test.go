//go:build darwin

package machineguard

import (
	"net"
	"os"
	"slices"
	"testing"
	"time"
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

// TestPCBListAttributionBudget (opt-in: PB_PCB_SCALE=1, needs a raised fd limit) holds loopback connections and
// times attribution. macOS budget (F23-01): <=2 ms worst at 1,000 sockets, <=15 ms worst at 10,000 sockets; the
// pcb-list copy is linear in the host's sockets and macOS has no per-connection lookup.
func TestPCBListAttributionBudget(t *testing.T) {
	if os.Getenv("PB_PCB_SCALE") != "1" {
		t.Skip("set PB_PCB_SCALE=1 to hold up to 10,000 sockets")
	}
	for _, c := range []struct {
		pairs  int
		budget time.Duration
	}{{500, 2 * time.Millisecond}, {5000, 15 * time.Millisecond}} {
		worst, median := attributionTimes(t, c.pairs)
		t.Logf("%d sockets: attribution median %v worst %v (budget %v)", 2*c.pairs, median, worst, c.budget)
		if worst > c.budget {
			t.Errorf("%d sockets: worst attribution %v is over the %v budget", 2*c.pairs, worst, c.budget)
		}
	}
}

func attributionTimes(t *testing.T, pairs int) (worst, median time.Duration) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			// a reset instead of TIME_WAIT, so the next case scans only its own sockets
			_ = c.(*net.TCPConn).SetLinger(0)
			c.Close()
		}
	}()
	for i := 0; i < pairs; i++ {
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
	var all []time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		owners, err := lookupOwnersPCBList(tuple)
		all = append(all, time.Since(start))
		if err != nil || len(owners) != 1 || owners[0].UID != uint32(os.Getuid()) {
			t.Fatalf("owners=%+v err=%v", owners, err)
		}
	}
	slices.Sort(all)
	return all[len(all)-1], all[len(all)/2]
}
