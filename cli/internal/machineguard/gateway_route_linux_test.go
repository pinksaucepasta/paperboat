package machineguard

import (
	"bufio"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// F23-01: one gateway listener shared by two OS users hands each connection to the daemon of the uid that opened
// it, never to the other; a uid without a route gets the refusal page on port 80 and reaches no daemon.
func TestGatewayRoutesEachConnectionToItsUsersDaemon(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	owners := map[uint16]uint32{} // client port → uid, as the kernel would answer
	lookup := func(tuple socketTuple) ([]socketOwner, error) {
		mu.Lock()
		defer mu.Unlock()
		uid, ok := owners[tuple.Source.Port()]
		if !ok {
			return nil, nil
		}
		return []socketOwner{{Tuple: tuple, UID: uid, Established: true}}, nil
	}
	gateway := &sharedGateway{port: 80, listener: listener, routes: map[uint32]*gatewayRoute{}, slots: make(chan struct{}, maxGatewayAttributions), lookup: lookup, release: func(*sharedGateway) {}}
	go gateway.serve()
	defer listener.Close()

	daemons := map[uint32]net.Listener{}
	for _, uid := range []uint32{1001, 1002} {
		route, err := gateway.addRoute(uid)
		if err != nil {
			t.Fatal(err)
		}
		defer route.Close()
		file, err := route.File()
		if err != nil {
			t.Fatal(err)
		}
		daemon, err := newHandoffListener(file, netip.MustParseAddr("127.0.0.1"), 80)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		defer daemon.Close()
		daemons[uid] = daemon
	}
	dial := func(uid uint32, known bool) net.Conn {
		t.Helper()
		// Bind first so the fake kernel knows the client port before the guard looks it up.
		local, err := net.ResolveTCPAddr("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		probe, err := net.ListenTCP("tcp4", local)
		if err != nil {
			t.Fatal(err)
		}
		port := probe.Addr().(*net.TCPAddr).Port
		probe.Close()
		if known {
			mu.Lock()
			owners[uint16(port)] = uid
			mu.Unlock()
		}
		dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, Timeout: 5 * time.Second}
		conn, err := dialer.Dial("tcp4", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	for _, uid := range []uint32{1001, 1002, 1001} {
		client := dial(uid, true)
		marker := []byte{byte(uid % 256)}
		if _, err := client.Write(marker); err != nil {
			t.Fatal(err)
		}
		accepted, err := acceptWithin(daemons[uid], 5*time.Second)
		if err != nil {
			t.Fatalf("uid %d: its daemon got no connection: %v", uid, err)
		}
		got := make([]byte, 1)
		if _, err := io.ReadFull(accepted, got); err != nil || got[0] != marker[0] {
			t.Fatalf("uid %d: daemon read %v, %v; want its own marker", uid, got, err)
		}
		accepted.Close()
		client.Close()
	}
	// uid 1003 has no route and uid 1004 cannot be attributed: both get the refusal page, no daemon sees them.
	for _, c := range []struct {
		uid   uint32
		known bool
	}{{1003, true}, {1004, false}} {
		client := dial(c.uid, c.known)
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		status, err := bufio.NewReader(client).ReadString('\n')
		client.Close()
		if err != nil || !strings.Contains(status, "403") {
			t.Fatalf("uid %d: got %q, %v; want the refusal page", c.uid, status, err)
		}
	}
	for uid, daemon := range daemons {
		if conn, err := acceptWithin(daemon, 200*time.Millisecond); err == nil {
			conn.Close()
			t.Fatalf("daemon of uid %d received a connection it does not own", uid)
		}
	}
}

func acceptWithin(listener net.Listener, wait time.Duration) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := listener.Accept()
		done <- result{conn, err}
	}()
	select {
	case r := <-done:
		return r.conn, r.err
	case <-time.After(wait):
		go func() {
			if r := <-done; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, io.ErrNoProgress
	}
}
