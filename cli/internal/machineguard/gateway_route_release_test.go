//go:build linux || darwin

package machineguard

import (
	"net"
	"testing"
)

// X-19: releasing a gateway port's last route through the guard's lease path (shutdownListener) closes the shared
// 127.100.0.1:<port> socket. macOS kept it bound with no routes, so a user's own service on that port failed to start.
func TestReleasingTheLastGatewayRouteClosesTheSharedListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	released := 0
	gateway := &sharedGateway{port: 720, listener: listener, routes: map[uint32]*gatewayRoute{}, release: func(*sharedGateway) { released++ }}
	first, err := gateway.addRoute(1001)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gateway.addRoute(1002)
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdownListener(first); err != nil {
		t.Fatal(err)
	}
	if released != 0 {
		t.Fatal("the shared gateway was released while another user's route still used it")
	}
	if err := shutdownListener(second); err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("the shared gateway was released %d times after its last route, want once", released)
	}
	if conn, err := net.Dial("tcp4", address); err == nil {
		conn.Close()
		t.Fatal("the shared gateway socket still accepts connections after its last route was released")
	}
}
