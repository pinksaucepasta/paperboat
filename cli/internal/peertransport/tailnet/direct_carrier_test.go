package tailnet

import "testing"

func TestDirectCarrierRetainsAuthorityWithoutPinningWireGuardState(t *testing.T) {
	a, configuration, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	direct, backing, err := a.TrackAdmittedCarrier("machine_test", false)
	if err != nil {
		t.Fatal(err)
	}
	defer direct()
	if a.carrierPeers["machine_test"] != 0 {
		t.Fatal("direct QUIC transport pinned the old WireGuard key state")
	}
	if len(a.AdmittedScopes()) != 1 {
		t.Fatal("direct transport lost its signed grant retention")
	}
	wireguard, _, err := a.TrackAdmittedCarrier("machine_test", true)
	if err != nil {
		t.Fatal(err)
	}
	if a.carrierPeers["machine_test"] != 1 {
		t.Fatal("live WireGuard transport was not retained")
	}
	wireguard()
	if a.carrierPeers["machine_test"] != 0 || len(a.AdmittedScopes()) != 1 {
		t.Fatal("closing WireGuard did not leave only direct authority")
	}
	peer := configuration.Peers[0].Identity
	configuration.Generation++
	configuration.Peers = nil
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	if a.AllowsAdmittedCarrier("machine_test", peer, backing) {
		t.Fatal("direct transport survived signed withdrawal")
	}
	direct()
	if len(a.AdmittedScopes()) != 0 {
		t.Fatal("direct transport left admitted scope references")
	}
}
