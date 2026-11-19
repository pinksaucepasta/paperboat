//go:build linux || darwin

package machineguard

import (
	"net"
	"testing"
)

// freeTCPPorts returns n distinct ports nothing listens on, on any address. The guard takes the machine port as
// a number and binds it itself, so the port cannot be handed over as an open listener; kernel-chosen ports
// replace the fixed numbers that collided with whatever else listened on them.
func freeTCPPorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, n)
	for i := range ports {
		l, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close() // held until all are chosen, so no two are equal
		ports[i] = l.Addr().(*net.TCPAddr).Port
	}
	return ports
}
