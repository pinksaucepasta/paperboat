package peersessions

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func TestWireGuardPublicKeyRejectsLowOrderInputs(t *testing.T) {
	if validWireGuardPublicKey(make([]byte, 32)) {
		t.Fatal("accepted all-zero low-order public key")
	}
	if !validWireGuardPublicKey(bytes.Repeat([]byte{7}, 32)) {
		t.Fatal("rejected non-low-order public key")
	}
}

func TestPersonalNetworkScopeHasReciprocalPairAndNoApplicationCapability(t *testing.T) {
	expiry := time.Now().Add(time.Minute)
	for _, roles := range [][2]string{{"cli", "machine"}, {"machine", "machine"}} {
		a := networkBinding{AccountID: "account", EndpointID: "a", Role: roles[0]}
		b := networkBinding{AccountID: "account", EndpointID: "b", Role: roles[1]}
		first, second := personalNetworkScope(a, b, expiry), personalNetworkScope(b, a, expiry)
		if first.ResourceID != second.ResourceID || first.Direction == second.Direction || first.Capability != "connect" || first.ResourceKind != "machine_network" || first.ExpiresAt != expiry.Unix() {
			t.Fatalf("nonreciprocal network scope: %+v %+v", first, second)
		}
		a.AccountID = "other"
		if personalNetworkScope(a, b, expiry).ResourceID == first.ResourceID {
			t.Fatal("pair identifier is not account bound")
		}
	}
}

func TestRelayGrantKeepsThePeersInUseAheadOfTheBound(t *testing.T) {
	self := networkBinding{AccountID: "account", EndpointID: "self", Role: "machine"}
	var peers []networkPeer
	for i := range maximumRelayGrantPeers + 6 {
		id := fmt.Sprintf("peer_%03d", i)
		peers = append(peers, networkPeer{Identity: networkBinding{AccountID: "account", EndpointID: id, Role: "machine"}, Scopes: []networkScope{{ResourceKind: "machine_network"}}})
	}
	last := peers[len(peers)-1].Identity
	sessionPeer := peers[len(peers)-2].Identity.EndpointID
	peers[len(peers)-2].Scopes = append(peers[len(peers)-2].Scopes, networkScope{ResourceKind: "machine_access"})
	ordered := relayPriority(self, peers, map[string]bool{}, []string{personalNetworkScopeID(self, last)})
	kept := map[string]bool{}
	for _, peer := range ordered[:maximumRelayGrantPeers] {
		kept[peer.Identity.EndpointID] = true
	}
	if !kept[last.EndpointID] || !kept[sessionPeer] {
		t.Fatalf("bounded relay grant dropped a peer in use: live network %v, session %v", kept[last.EndpointID], kept[sessionPeer])
	}
	if len(relayGrantPeers(ordered)) != maximumRelayGrantPeers {
		t.Fatal("relay grant is not bounded")
	}
}
