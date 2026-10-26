package machineguard

import (
	"errors"
	"slices"
	"testing"
)

// lanea-r4, 2026-10-27: a daemon and guard of different versions refused each other (the older guard
// decodes requests strictly; the newer guard required account and machine). A request without them, from an
// older daemon, is still served: as that uid's claim on that exact address, refused if another holds it.
func TestGuardServesAMachineListenerRequestWithoutAccount(t *testing.T) {
	g := &guardServer{cfg: Config{StateDir: t.TempDir()}, reserved: reservations{Names: map[string]string{}, Claims: map[string]claim{}}}
	g.claims = &claimBook{Claims: g.reserved.Claims}
	older := request{Operation: "acquire", Hostname: "hp.local.example.test", IP: "127.100.0.7", Port: 22}
	got, err := g.assignClaim("1001", older)
	if err != nil || got.IP != "127.100.0.7" {
		t.Fatalf("older daemon's exact address: ip=%q err=%v", got.IP, err)
	}
	if again, err := g.assignClaim("1001", older); err != nil || again.IP != "127.100.0.7" {
		t.Fatalf("the same uid asking again: ip=%q err=%v", again.IP, err)
	}
	if _, err := g.assignClaim("1002", older); !errors.Is(err, ErrListenerRefused) {
		t.Fatalf("another uid took the address: %v", err)
	}
	newer := request{Operation: "acquire", Account: "acct", Machine: "m", Hostname: "hp.local.example.test", IP: "127.100.0.7", Port: 22}
	if moved, err := g.assignClaim("1002", newer); err != nil || moved.IP == "127.100.0.7" {
		t.Fatalf("a newer daemon of another uid was not given another address: ip=%q err=%v", moved.IP, err)
	}
	if status := g.runtimeStatus(); !slices.Contains(status.Features, FeatureAddressClaims) || slices.Contains(status.Features, FeatureGatewayRouting) != gatewayRouting {
		t.Fatalf("status features=%v", status.Features)
	}
}
