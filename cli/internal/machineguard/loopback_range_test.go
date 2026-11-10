//go:build linux

package machineguard

import (
	"context"
	"testing"
)

// The machine range is fixed at 127.100.0.0/16: an address in any other range is refused for active forwarding.
func TestGuardRefusesAddressesOutsideTheFixedRange(t *testing.T) {
	owner := &testControlConn{identity: "0"}
	g := &guardServer{cfg: Config{LoopbackCIDR: "127.100.0.0/16"}, leases: map[string]*guardedLease{}, connections: map[controlConn]string{owner: "0"}}
	if _, err := g.handle(context.Background(), owner, "0", request{Operation: "acquire", Hostname: "office.local.example.test", IP: "127.212.23.45", Port: 8080}); err == nil {
		t.Fatal("an address outside the fixed range was accepted for active forwarding")
	}
}
