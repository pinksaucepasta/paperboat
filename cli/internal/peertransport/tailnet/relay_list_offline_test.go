package tailnet

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// F21-49: while renewals fail, the last signed relay list is still dialed after its expiry, so relays come back the
// moment the network does, but only with a relay grant that has not expired (the relay enforces the grant), and only
// until a renewal answers.
func TestExpiredRelayListIsDialedOnlyInsideTheGrantWhileRenewalsFail(t *testing.T) {
	a, configuration, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	signed := now - 300 // the list was signed before a five-minute outage and expired during it
	node := RegionalNode{
		NodeID: "relay_test", NodeGeneration: 1, ProcessEpoch: "epoch_test", Region: "test", FailureDomain: "domain_test",
		Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: 443,
		State: "ready", ObservedAt: signed, ExpiresAt: signed + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: signed,
	}
	r := &regionalRecovery{authority: a}
	relays := func(failing bool, grantExpires int64) int {
		t.Helper()
		a.mu.Lock()
		a.regional = &RegionalCandidates{AuthorizationGeneration: configuration.Generation, IssuedAt: signed, ExpiresAt: signed + 30, Nodes: []RegionalNode{node}}
		a.mu.Unlock()
		a.relay.mu.Lock()
		a.relay.grants = map[string]peernetwork.RelayGrant{node.NodeID: {NodeID: node.NodeID, NodeGeneration: 1, ProcessEpoch: "epoch_test", Generation: configuration.Generation, ExpiresAt: grantExpires}}
		a.relay.mu.Unlock()
		a.renewalFailing.Store(failing)
		nodes, _, _, err := r.snapshot()
		if err != nil {
			return 0
		}
		return len(nodes)
	}
	if n := relays(true, now+3600); n != 1 {
		t.Fatalf("renewals failing, grant valid: %d relays, want the last known one", n)
	}
	if n := relays(true, now-1); n != 0 {
		t.Fatalf("renewals failing, grant expired: %d relays, want none", n)
	}
	if n := relays(false, now+3600); n != 0 {
		t.Fatalf("renewal answered: %d relays from the expired list, want none", n)
	}
}
