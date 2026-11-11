package tailnet

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// CI run 42002892817: a refresh's candidates moved relay b to a new port before its grants arrived. A
// snapshot in between paired the new address with the old refresh's grant, a carrier dialled it, and the
// relay fenced the replaced grant for good ("fence (generation 1, held 2)").
func TestMovedRelayKeepsItsAddressUntilItsGrantFromTheSameRefresh(t *testing.T) {
	a, configuration, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{
		NodeID: "relay_test", NodeGeneration: 1, ProcessEpoch: "epoch_test", Region: "test", FailureDomain: "domain_test",
		Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: 443,
		State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now,
	}
	moved := node
	moved.EndpointQUICPort = 444
	r := &regionalRecovery{authority: a}
	state := func(generation uint64, candidate RegionalNode, grantGeneration uint64) uint16 {
		t.Helper()
		a.mu.Lock()
		a.regional = &RegionalCandidates{AuthorizationGeneration: generation, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{candidate}}
		a.mu.Unlock()
		a.relay.mu.Lock()
		a.relay.grants = map[string]peernetwork.RelayGrant{node.NodeID: {NodeID: node.NodeID, NodeGeneration: 1, ProcessEpoch: "epoch_test", Generation: grantGeneration, ExpiresAt: now + 60}}
		a.relay.mu.Unlock()
		nodes, _, _, err := r.snapshot()
		if err != nil || len(nodes) != 1 {
			t.Fatalf("snapshot: %v, %v", nodes, err)
		}
		return nodes[0].EndpointQUICPort
	}
	g := configuration.Generation
	if port := state(g, node, g); port != 443 {
		t.Fatalf("current relay port %d", port)
	}
	if port := state(g+1, moved, g); port != 443 {
		t.Fatalf("moved relay published with the replaced grant: port %d", port)
	}
	if port := state(g+1, moved, g+1); port != 444 {
		t.Fatalf("moved relay not published with its own grant: port %d", port)
	}
}
