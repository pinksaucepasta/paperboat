package derpquic

import (
	"context"
	"testing"

	"tailscale.com/types/key"
)

func TestRegisteredServiceReadiness(t *testing.T) {
	f := newFixture(t)
	server, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	identity := ServiceDescriptor{WireGuardPublicKey: KeyString(key.NewNode().Public()), DiscoPublicKey: DiscoKeyString(key.NewDisco().Public()), VirtualAddress: "fd7a:115c:a1e0::123"}
	if !server.MatchesControlService(nil) || server.MatchesControlService(&identity) {
		t.Fatal("missing service advertised ready")
	}
	if err = server.SetControlService(identity, func(context.Context, ControlRequest) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	wrong := identity
	wrong.DiscoPublicKey = DiscoKeyString(key.NewDisco().Public())
	if server.MatchesControlService(nil) || server.MatchesControlService(&wrong) || !server.MatchesControlService(&identity) {
		t.Fatal("service readiness ignored registered identity")
	}
}
