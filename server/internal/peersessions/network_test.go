package peersessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
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
		a := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "a", Role: roles[0]}}
		b := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "b", Role: roles[1]}}
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
	self := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "self"}}
	var peers []networkPeer
	for i := range 100 {
		account := "account"
		if i%10 == 0 {
			account = "other" // a team peer of another account: named, never a control peer
		}
		peers = append(peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: account, EndpointID: fmt.Sprintf("peer_%03d", i), WireGuardPublicKey: fmt.Sprintf("key_%03d", (i*37)%1000)}},
			Scopes: []networkScope{{ResourceKind: "machine_network", ResourceID: fmt.Sprintf("network_%03d", i), ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: 1}}})
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
				key := entry.WireGuardPublicKey
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
	self := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "self"}}
	untilRevoked := networkScope{ResourceKind: "machine_network", ResourceID: "network_x", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: 0}
	peers := []networkPeer{
		{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "keyed", WireGuardPublicKey: "key_a"}}, Scopes: []networkScope{untilRevoked}},
		{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "keyless"}}, Scopes: []networkScope{untilRevoked}},
	}
	chunks := relayGrantChunks(self, peers, maximumRelayGrantPeers, 1234)
	if len(chunks) != 1 || len(chunks[0].peers) != 1 || len(chunks[0].control) != 1 || chunks[0].control[0] != "key_a" {
		t.Fatalf("chunks %+v: want the keyed peer only", chunks)
	}
	scopes := chunks[0].peers[0].Scopes
	if len(scopes) != 1 || scopes[0].ExpiresAt != 1234 {
		t.Fatalf("grant scopes %+v: want expires_at = grant expiry", scopes)
	}
	if peers[0].Scopes[0].ExpiresAt != 0 {
		t.Fatal("the document's scope was changed in place")
	}
}

// P0 2026-10-26: under until_revoked_v1 every CLI peer of a host also carried its offline session rights
// (machine_access, expires_at 0), so a 64-peer grant went past the relay's 128-scope bound and pairing failed. The
// account's own peers pair by machine_network, so their session rights stay out; a team member's CLI (another
// account, no machine_network scope) is paired by its standing machine_access scopes, which a grant carries with the
// grant expiry (F5-10: a member's ssh to a NAT'd team Mac timed out because no relay grant named the pair).
// Chunks stay within the relay's 128-scope bound and name each peer once.
func TestRelayGrantsCarryStandingTeamScopesAndStayWithinTheRelayScopeBound(t *testing.T) {
	self := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "host"}}
	var peers []networkPeer
	for i := range 130 {
		scopes := []networkScope{
			{ResourceKind: "machine_network", ResourceID: fmt.Sprintf("network_%03d", i), ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: 0},
			{ResourceKind: "machine_network", ResourceID: fmt.Sprintf("network_%03d", i), ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443, ExpiresAt: 0},
		}
		for _, capability := range []string{"terminal", "exec", "managed_ssh", "file_transfer"} {
			scopes = append(scopes, networkScope{ResourceKind: "machine_access", ResourceID: "host", ResourceGeneration: 7, Capability: capability, Direction: "accept", Port: 443, ExpiresAt: 0})
		}
		identity := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: fmt.Sprintf("cli_%03d", i), WireGuardPublicKey: fmt.Sprintf("key_%03d", i)}}
		peers = append(peers, networkPeer{Identity: identity, Scopes: scopes})
	}
	// Team members' CLIs hold only standing session rights on this host (inspector is no relay capability).
	for i := range 64 {
		var scopes []networkScope
		for _, capability := range []string{"terminal", "exec", "managed_ssh", "file_transfer", "inspector"} {
			scopes = append(scopes, networkScope{ResourceKind: "machine_access", ResourceID: "host", ResourceGeneration: 7, Capability: capability, Direction: "accept", Port: 443, ExpiresAt: 0})
		}
		scopes = append(scopes, networkScope{ResourceKind: "machine_access", ResourceID: "host", ResourceGeneration: 7, Capability: "private_access", Direction: "accept", Port: 443, ExpiresAt: 0},
			networkScope{ResourceKind: "machine_access", ResourceID: "host", ResourceGeneration: 0, Capability: "exec", Direction: "accept", Port: 443, ExpiresAt: 0}, networkScope{ResourceKind: "machine_access", ResourceID: "host", ResourceGeneration: 7, Capability: "exec", Direction: "accept", Port: 80, ExpiresAt: 0})
		peers = append(peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "other", EndpointID: fmt.Sprintf("member_%03d", i), WireGuardPublicKey: fmt.Sprintf("member_%03d", i)}}, Scopes: scopes})
	}
	chunks := relayGrantChunks(self, peers, maximumRelayGrantPeers, 1234)
	named, members := map[string]bool{}, 0
	for _, chunk := range chunks {
		total := 0
		for _, entry := range chunk.peers {
			key := entry.WireGuardPublicKey
			if named[key] {
				t.Fatalf("peer %s named in two chunks", key)
			}
			named[key] = true
			scopes := entry.Scopes
			total += len(scopes)
			if strings.HasPrefix(key, "member_") {
				members++
				if len(scopes) != 4 {
					t.Fatalf("member %s carries %+v, want its 4 relay capabilities", key, scopes)
				}
			}
			for _, scope := range scopes {
				if scope.ExpiresAt != 1234 {
					t.Fatalf("grant scope %+v: want an expiry equal to the grant's", scope)
				}
				if strings.HasPrefix(key, "key_") && scope.ResourceKind != "machine_network" {
					t.Fatalf("account peer %s carries session right %+v", key, scope)
				}
			}
		}
		if total != chunk.scopes || total > maximumRelayGrantScopes || len(chunk.peers) > maximumRelayGrantPeers {
			t.Fatalf("a grant holds %d scopes (counted %d) for %d peers, the relay accepts 128 scopes", total, chunk.scopes, len(chunk.peers))
		}
	}
	if len(named) != 130+64 || members != 64 {
		t.Fatalf("%d peers named (%d members), want 130 account peers and 64 members", len(named), members)
	}
	if len(chunks) > 32 {
		t.Fatalf("%d chunks, a credential carries at most 32 parts", len(chunks))
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
			unlock, err := locks.lock(t.Context(), "endpoint")
			if err != nil {
				t.Error(err)
				return
			}
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

// A stalled build must not hold canceled refresh requests in its wait queue.
func TestConfigurationBuildLockHonorsCancellation(t *testing.T) {
	s := &NetworkService{}
	unlock, err := s.endpointBuilds.lock(t.Context(), "endpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Configuration(ctx, NetworkConfigRequest{EndpointID: "endpoint"}); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Configuration = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request still waits for the active build")
	}
	s.endpointBuilds.mu.Lock()
	defer s.endpointBuilds.mu.Unlock()
	if s.endpointBuilds.locks["endpoint"].refs != 1 {
		t.Fatal("canceled waiter retained a lock reference")
	}
}
