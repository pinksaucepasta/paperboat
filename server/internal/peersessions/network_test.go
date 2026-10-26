package peersessions

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
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
		first, second := personalNetworkScope(a, b, expiry.Unix()), personalNetworkScope(b, a, expiry.Unix())
		if first.ResourceID != second.ResourceID || first.Direction == second.Direction || first.Capability != "connect" || first.ResourceKind != "machine_network" || first.ExpiresAt != expiry.Unix() {
			t.Fatalf("nonreciprocal network scope: %+v %+v", first, second)
		}
		a.AccountID = "other"
		if personalNetworkScope(a, b, expiry.Unix()).ResourceID == first.ResourceID {
			t.Fatal("pair identifier is not account bound")
		}
	}
}

// Every peer is named in exactly one grant, and every same-account peer's control key rides in the grant that
// names it, within the per-grant bound released relays and CLIs accept (Lane W: 100-peer accounts got 16).
func TestRelayGrantsCarryEveryPeerAndItsControlKeyInChunksOfTheRelayBound(t *testing.T) {
	self := networkBinding{AccountID: "account", EndpointID: "self"}
	var peers []networkPeer
	for i := range 100 {
		account := "account"
		if i%10 == 0 {
			account = "other" // a team peer of another account: named, never a control peer
		}
		peers = append(peers, networkPeer{Identity: networkBinding{AccountID: account, EndpointID: fmt.Sprintf("peer_%03d", i), WireGuardPublicKey: fmt.Sprintf("key_%03d", (i*37)%1000)}})
	}
	for _, size := range []int{maximumRelayControlPeers, maximumRelayGrantPeers} {
		chunks := relayGrantChunks(self, peers, size, 1)
		if want := (len(peers) + size - 1) / size; len(chunks) != want {
			t.Fatalf("size %d: chunks = %d, want %d", size, len(chunks), want)
		}
		named, control := map[string]bool{}, map[string]bool{}
		for _, chunk := range chunks {
			if len(chunk.peers) == 0 || len(chunk.peers) > size || len(chunk.control) > size {
				t.Fatalf("chunk of %d peers and %d control keys", len(chunk.peers), len(chunk.control))
			}
			inChunk := map[string]bool{}
			for _, entry := range chunk.peers {
				key := entry["wireguard_public_key"].(string)
				if named[key] {
					t.Fatalf("peer %s in two chunks", key)
				}
				named[key], inChunk[key] = true, true
			}
			for _, key := range chunk.control {
				if !inChunk[key] || control[key] {
					t.Fatalf("control key %s outside its peer's chunk or repeated", key)
				}
				control[key] = true
			}
		}
		if len(named) != 100 || len(control) != 90 {
			t.Fatalf("size %d: named %d of 100 peers, %d of 90 control keys", size, len(named), len(control))
		}
	}
	if empty := relayGrantChunks(self, nil, maximumRelayGrantPeers, 1); len(empty) != 1 || len(empty[0].peers) != 0 {
		t.Fatal("an endpoint without peers still gets one grant per node")
	}
}

// A daemon holding peers until revoked gets scopes with expires_at 0; a relay grant (and mint, and released relays)
// requires an expiry, so inside a grant such a scope ends with the grant. A peer without a WireGuard key is left out
// instead of failing every other peer's grant (P0 2026-10-26: one bad peer made every until_revoked_v1 config 503).
func TestRelayGrantsGiveUntilRevokedScopesTheGrantExpiryAndOmitKeylessPeers(t *testing.T) {
	self := networkBinding{AccountID: "account", EndpointID: "self"}
	untilRevoked := networkScope{"machine_network", "network_x", 1, "connect", "dial", 443, 0}
	peers := []networkPeer{
		{Identity: networkBinding{AccountID: "account", EndpointID: "keyed", WireGuardPublicKey: "key_a"}, Scopes: []networkScope{untilRevoked}},
		{Identity: networkBinding{AccountID: "account", EndpointID: "keyless"}, Scopes: []networkScope{untilRevoked}},
	}
	chunks := relayGrantChunks(self, peers, maximumRelayGrantPeers, 1234)
	if len(chunks) != 1 || len(chunks[0].peers) != 1 || len(chunks[0].control) != 1 || chunks[0].control[0] != "key_a" {
		t.Fatalf("chunks %+v: want the keyed peer only", chunks)
	}
	scopes := chunks[0].peers[0]["scopes"].([]networkScope)
	if len(scopes) != 1 || scopes[0].ExpiresAt != 1234 {
		t.Fatalf("grant scopes %+v: want expires_at = grant expiry", scopes)
	}
	if peers[0].Scopes[0].ExpiresAt != 0 {
		t.Fatal("the document's scope was changed in place")
	}
}

func TestKeyedLocksSerializeOneKeyAndForgetIt(t *testing.T) {
	var locks keyedLocks
	var inside, maximum int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := locks.lock("endpoint")
			if n := atomic.AddInt32(&inside, 1); n > atomic.LoadInt32(&maximum) {
				atomic.StoreInt32(&maximum, n)
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
			unlock()
		}()
	}
	wg.Wait()
	if maximum != 1 {
		t.Fatalf("%d builds of one endpoint ran at once", maximum)
	}
	if len(locks.locks) != 0 {
		t.Fatal("lock entries outlived their holders")
	}
}
