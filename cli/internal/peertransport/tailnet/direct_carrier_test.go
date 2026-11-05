package tailnet

import "testing"

func TestAdmittedCarrierRetainsWireGuardStateUntilSignedWithdrawal(t *testing.T) {
	a, configuration, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	release, backing, err := a.TrackAdmittedCarrier("machine_test")
	if err != nil {
		t.Fatal(err)
	}
	if a.carrierPeers["machine_test"] != 1 || len(a.AdmittedScopes()) != 1 {
		t.Fatal("live WireGuard transport was not retained")
	}
	peer := configuration.Peers[0].Identity
	configuration.Generation++
	configuration.Peers = nil
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	if a.AllowsAdmittedCarrier("machine_test", peer, backing) {
		t.Fatal("carrier survived signed withdrawal")
	}
	release()
	if a.carrierPeers["machine_test"] != 0 || len(a.AdmittedScopes()) != 0 {
		t.Fatal("closing the carrier left admitted references")
	}
}
