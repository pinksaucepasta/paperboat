package machineguard

import (
	"errors"
	"slices"
	"testing"
)

// A machine address request names the account and the machine its address is claimed for; without them it is
// refused, and one uid never receives another uid's claimed address.
func TestGuardRefusesAMachineListenerRequestWithoutAccount(t *testing.T) {
	g := &guardServer{cfg: Config{StateDir: t.TempDir()}, reserved: reservations{Names: map[string]string{}, Claims: map[string]claim{}}}
	g.claims = &claimBook{Claims: g.reserved.Claims}
	without := request{Operation: "acquire", Hostname: "hp.local.example.test", IP: "127.100.0.7", Port: 22}
	if _, err := g.assignClaim("1001", without); !errors.Is(err, ErrListenerRefused) {
		t.Fatalf("a request without account and machine was served: %v", err)
	}
	first := request{Operation: "acquire", Account: "acct", Machine: "m", Hostname: "hp.local.example.test", IP: "127.100.0.7", Port: 22}
	if got, err := g.assignClaim("1001", first); err != nil || got.IP != "127.100.0.7" {
		t.Fatalf("the preferred address: ip=%q err=%v", got.IP, err)
	}
	newer := first
	if moved, err := g.assignClaim("1002", newer); err != nil || moved.IP == "127.100.0.7" {
		t.Fatalf("another uid was not given the claimed address: ip=%q err=%v", moved.IP, err)
	}
	if status := g.runtimeStatus(); !slices.Contains(status.Features, FeatureAddressClaims) || slices.Contains(status.Features, FeatureGatewayRouting) != gatewayRouting {
		t.Fatalf("status features=%v", status.Features)
	}
}
