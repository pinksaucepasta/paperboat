//go:build linux || darwin || windows

package machineguard

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
)

func TestGatewayServesEveryPort(t *testing.T) {
	for port, want := range map[int]bool{0: false, 22: true, 79: true, 80: true, 443: true, 444: true, 1023: true, 1024: true, 5432: true, 65535: true, 65536: false} {
		if gatewayPortAllowed(port) != want {
			t.Errorf("gatewayPortAllowed(%d)=%v, want %v", port, !want, want)
		}
	}
	gateway := splitdns.BrowserGatewayIP
	// :0 is the guard's own free-port bind behind its redirect; requests for port 0 are refused above.
	if !validProtectedBindAddress(gateway+":5432", machineloopback.DefaultCIDR) || !validProtectedBindAddress(gateway+":22", machineloopback.DefaultCIDR) || !validProtectedBindAddress(gateway+":0", machineloopback.DefaultCIDR) || validProtectedBindAddress(gateway+":65536", machineloopback.DefaultCIDR) {
		t.Fatal("protected bind addresses on the gateway do not follow the gateway port policy (any port, 22 included)")
	}
}
