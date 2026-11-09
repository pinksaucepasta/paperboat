//go:build darwin

package machineguard

import (
	"encoding/binary"
	"errors"
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

// TestPCBListAttributesAnIPv6LoopbackConnectionToItsUID is the F23-02 check of the ::1 layout (inp_dependfaddr and
// inp_dependladdr at 48 and 64), which was only read from xnu's headers until a live Mac ran it. On failure it logs
// the pcb records that carry the connection's ports, so a wrong offset can be read off the bytes.
func TestPCBListAttributesAnIPv6LoopbackConnectionToItsUID(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	defer listener.Close()
	client, err := net.Dial("tcp6", listener.Addr().String())
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
		t.Errorf("attributed uid=%d err=%v; want %d for %v", uid, err, os.Getuid(), tuple)
		logPCBRecords(t, tuple)
	}
}

// TestPCBListAttributionUnderTupleReuseChurn is the macOS twin of the Linux churn test (F23-02): a client port reused
// after a reset reaches the gateway as the same tuple, and each connection is attributed to its user only while its
// own connecting socket is live; the reset client's stale accepted connection is refused, never given a uid.
func TestPCBListAttributionUnderTupleReuseChurn(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		host := map[string]string{"tcp4": "127.0.0.1", "tcp6": "[::1]"}[network]
		listener, err := net.Listen(network, host+":0")
		if err != nil {
			t.Skipf("no %s loopback", network)
		}
		probe, err := net.Listen(network, host+":0")
		if err != nil {
			t.Fatal(err)
		}
		port := probe.Addr().(*net.TCPAddr).Port
		_ = probe.Close()
		dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: listener.Addr().(*net.TCPAddr).IP, Port: port}}
		var previous string
		for round := 0; round < 100; round++ {
			accepted := make(chan net.Conn, 1)
			go func() {
				c, _ := listener.Accept()
				accepted <- c
			}()
			client, err := dialer.Dial(network, listener.Addr().String())
			if err != nil {
				t.Fatalf("%s round %d: %v", network, round, err)
			}
			server := <-accepted
			tuple, err := clientTuple(server)
			if err != nil {
				t.Fatal(err)
			}
			if uid, err := attributeOwner(tuple, lookupOwnersPCBList); err != nil || uid != uint32(os.Getuid()) {
				t.Fatalf("%s round %d: live connection = uid %d, %v", network, round, uid, err)
			}
			if previous != "" && previous != server.RemoteAddr().String() {
				t.Fatalf("%s round %d: tuple was not reused", network, round)
			}
			previous = server.RemoteAddr().String()
			_ = client.(*net.TCPConn).SetLinger(0)
			_ = client.Close()
			time.Sleep(time.Millisecond)
			if uid, err := attributeOwner(tuple, lookupOwnersPCBList); !errors.Is(err, ErrAttribution) {
				t.Fatalf("%s round %d: reset client still attributed: uid %d, %v", network, round, uid, err)
			}
			_ = server.Close()
		}
		_ = listener.Close()
	}
}

// logPCBRecords prints every XSO_INPCB record (104 bytes at kind 0x010) whose ports match the tuple.
func logPCBRecords(t *testing.T, tuple socketTuple) {
	buf, err := pcbSnapshots.get()
	if err != nil {
		t.Logf("pcblist: %v", err)
		return
	}
	for off := 24; off+8 <= len(buf); {
		n, kind := int(binary.LittleEndian.Uint32(buf[off:])), binary.LittleEndian.Uint32(buf[off+4:])
		if n == 24 || n < 8 || off+n > len(buf) {
			return
		}
		if kind == 0x010 && n == 104 {
			r := buf[off : off+n]
			fport, lport := binary.BigEndian.Uint16(r[16:]), binary.BigEndian.Uint16(r[18:])
			if fport == tuple.Source.Port() || fport == tuple.Destination.Port() || lport == tuple.Source.Port() || lport == tuple.Destination.Port() {
				t.Logf("inpcb vflag=%#x fport=%d lport=%d bytes=%x", r[44], fport, lport, r)
			}
		}
		off += (n + 7) &^ 7
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
