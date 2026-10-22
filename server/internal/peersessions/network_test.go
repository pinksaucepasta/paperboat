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

func TestRelayGrantsCarryEveryPeerInChunksOfTheRelayBound(t *testing.T) {
	var peers []networkPeer
	for i := range 2*maximumRelayGrantPeers + 5 {
		peers = append(peers, networkPeer{Identity: networkBinding{EndpointID: fmt.Sprintf("peer_%03d", i), WireGuardPublicKey: fmt.Sprintf("key_%03d", (i*37)%1000)}})
	}
	chunks := relayGrantChunks(peers)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	seen := map[string]bool{}
	for _, chunk := range chunks {
		if len(chunk) == 0 || len(chunk) > maximumRelayGrantPeers {
			t.Fatalf("chunk of %d peers", len(chunk))
		}
		for _, entry := range chunk {
			key := entry["wireguard_public_key"].(string)
			if seen[key] {
				t.Fatalf("peer %s in two chunks", key)
			}
			seen[key] = true
		}
	}
	if len(seen) != len(peers) {
		t.Fatalf("chunks carry %d of %d peers", len(seen), len(peers))
	}
	if empty := relayGrantChunks(nil); len(empty) != 1 || len(empty[0]) != 0 {
		t.Fatal("an endpoint without peers still gets one grant per node")
	}
}
