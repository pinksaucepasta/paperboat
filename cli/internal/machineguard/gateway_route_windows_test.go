package machineguard

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testGateway(t *testing.T, address string, port int, owner func(net.Conn) (string, error), sids ...string) (net.Listener, map[string]*gatewayRoute) {
	t.Helper()
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	gateway := &sharedGateway{port: port, listener: listener, routes: map[string]*gatewayRoute{}, slots: make(chan struct{}, maxGatewayAttributions), owner: owner, release: func(*sharedGateway) {}}
	go gateway.serve()
	routes := map[string]*gatewayRoute{}
	for _, sid := range sids {
		routes[sid] = gateway.addRoute(sid)
		t.Cleanup(func() { routes[sid].Close() })
	}
	return listener, routes
}

// F23-01: one gateway listener shared by two OS users queues each connection for the daemon of the user that
// opened it, never the other's; a user without a route, an unattributable connection and a route whose daemon
// left get the refusal page on port 80 and reach no daemon.
func TestGatewayRoutesEachConnectionToItsUsersDaemon(t *testing.T) {
	const userA, userB, userC = "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-1002", "S-1-5-21-1-2-3-1003"
	var mu sync.Mutex
	owners := map[string]string{} // client address → SID, as attribution would answer
	owner := func(conn net.Conn) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if sid, ok := owners[conn.RemoteAddr().String()]; ok {
			return sid, nil
		}
		return "", errAttributionNotFound
	}
	listener, routes := testGateway(t, "127.0.0.1:0", 80, owner, userA, userB)
	dial := func(sid string) net.Conn {
		t.Helper()
		mu.Lock() // the gateway attributes only once the client's address is recorded
		defer mu.Unlock()
		conn, err := net.DialTimeout("tcp4", listener.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if sid != "" {
			owners[conn.LocalAddr().String()] = sid
		}
		return conn
	}
	refusedWithPage := func(sid string) {
		t.Helper()
		client := dial(sid)
		defer client.Close()
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		if status, err := bufio.NewReader(client).ReadString('\n'); err != nil || !strings.Contains(status, "403") {
			t.Fatalf("user %q: got %q, %v; want the refusal page", sid, status, err)
		}
	}
	for _, sid := range []string{userA, userB, userA} {
		client := dial(sid)
		if _, err := client.Write([]byte(sid + "\n")); err != nil {
			t.Fatal(err)
		}
		accepted, err := acceptWithin(routes[sid], 5*time.Second)
		if err != nil {
			t.Fatalf("user %s: its daemon got no connection: %v", sid, err)
		}
		if line, err := bufio.NewReader(accepted).ReadString('\n'); err != nil || strings.TrimSpace(line) != sid {
			t.Fatalf("user %s: its daemon read %q, %v", sid, line, err)
		}
		accepted.Close()
		client.Close()
	}
	refusedWithPage(userC) // no route
	refusedWithPage("")    // not attributable
	for sid, route := range routes {
		if conn, err := acceptWithin(route, 200*time.Millisecond); err == nil {
			conn.Close()
			t.Fatalf("daemon of %s received a connection it does not own", sid)
		}
	}
	routes[userB].Close()
	if _, err := routes[userB].Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed route accepted: %v", err)
	}
	refusedWithPage(userB) // its daemon left
	if conn, err := acceptWithin(routes[userA], 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("user A's daemon received user B's connection after B's daemon left")
	}
}

// A route that is closed with connections nobody accepted closes them; a full queue refuses instead of blocking.
func TestGatewayRouteQueueIsBoundedAndClosedWithTheRoute(t *testing.T) {
	route := (&sharedGateway{routes: map[string]*gatewayRoute{}}).addRoute("S-1-5-21-1-2-3-1001")
	var clients []net.Conn
	for i := 0; i < maxGatewayAttributions; i++ {
		client, server := net.Pipe()
		clients = append(clients, client)
		if !route.deliver(server) {
			t.Fatalf("connection %d was not queued", i)
		}
	}
	extra, server := net.Pipe()
	defer extra.Close()
	defer server.Close()
	if route.deliver(server) {
		t.Fatal("a full queue accepted another connection")
	}
	route.Close()
	if route.deliver(server) {
		t.Fatal("a closed route accepted a connection")
	}
	for i, client := range clients {
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("queued connection %d was not closed with the route: %v", i, err)
		}
	}
}

// F23-01 on a device: run as an administrator with PB_TWO_USER_SIDS=<sid>,<sid> and PB_TWO_USER_ADDRESS=<ip:port>
// while each of those OS users runs `curl http://<ip:port>/<its own SID>` in a loop (a new process per
// connection). The kernel-attributed gateway must queue each request on its own user's route only, 20 each, and
// refuse this process's own connections (a user without a route) with the refusal page.
func TestGatewayRoutesTwoOSUsersOnDevice(t *testing.T) {
	sids := strings.Split(os.Getenv("PB_TWO_USER_SIDS"), ",")
	address := os.Getenv("PB_TWO_USER_ADDRESS")
	if len(sids) != 2 || address == "" {
		t.Skip("needs PB_TWO_USER_SIDS=<sid>,<sid> and PB_TWO_USER_ADDRESS=<ip:port>")
	}
	listener, routes := testGateway(t, address, 80, connectionOwnerSID, sids...)
	results := make(chan error, len(sids))
	for sid, route := range routes {
		go func() {
			for seen := 0; seen < 20; seen++ {
				accepted, err := acceptWithin(route, 60*time.Second)
				if err != nil {
					results <- errors.New("user " + sid + ": its daemon got no connection: " + err.Error())
					return
				}
				_ = accepted.SetReadDeadline(time.Now().Add(5 * time.Second))
				line, err := bufio.NewReader(accepted).ReadString('\n')
				_, _ = io.WriteString(accepted, "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
				accepted.Close()
				if fields := strings.Fields(line); err != nil || len(fields) < 2 || fields[1] != "/"+sid {
					results <- errors.New("user " + sid + ": its daemon read " + line)
					return
				}
			}
			results <- nil
		}()
	}
	for range 20 {
		client, err := net.DialTimeout("tcp4", listener.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		status, err := bufio.NewReader(client).ReadString('\n')
		client.Close()
		if err != nil || !strings.Contains(status, "403") {
			t.Fatalf("a user without a route got %q, %v; want the refusal page", status, err)
		}
	}
	for range sids {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
