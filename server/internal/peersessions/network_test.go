package peersessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// A stalled build must not hold canceled refresh requests in its wait queue.
func TestConfigurationBuildLockHonorsCancellation(t *testing.T) {
	s := &NetworkService{}
	unlock, err := s.endpointBuilds.Lock(t.Context(), "endpoint")
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
}

// A relay grant carries memberships, not peers (PLAN 3b "Relay pair authorization by membership"): the account mesh,
// a per-pair scope per peer in the configuration, is one account network per direction this endpoint holds, with the
// grant's expiry for the until-revoked scopes; a peer of another account adds its relay scopes once each; a keyless
// peer adds nothing. 100 peers make 2 + 1 memberships, not 100 entries.
func TestRelayGrantMembershipsAreTheAccountNetworkAndEachOtherScopeOnce(t *testing.T) {
	self := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "self"}}
	var peers []networkPeer
	for i := range 100 {
		identity := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: fmt.Sprintf("peer_%03d", i), WireGuardPublicKey: fmt.Sprintf("key_%03d", i)}}
		scope := networkScope{ResourceKind: "machine_network", ResourceID: fmt.Sprintf("network_%03d", i), ResourceGeneration: 1, Capability: "connect", Direction: []string{"dial", "accept"}[i%2], Port: 443}
		if i%10 == 0 { // a team member of another account, sharing one access grant
			identity.AccountID = "other"
			scope = networkScope{ResourceKind: "machine_access", ResourceID: "access_1", ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: 99}
		}
		peers = append(peers, networkPeer{Identity: identity, Scopes: []networkScope{scope}})
	}
	peers = append(peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "other", EndpointID: "keyless"}},
		Scopes: []networkScope{{ResourceKind: "machine_access", ResourceID: "access_keyless", ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: 99}}})
	network := peernetwork.AccountNetworkID("account")
	want := []networkScope{
		{ResourceKind: "machine_network", ResourceID: network, ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443, ExpiresAt: 1234},
		{ResourceKind: "machine_network", ResourceID: network, ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: 1234},
		{ResourceKind: "machine_access", ResourceID: "access_1", ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: 99},
	}
	if got := relayGrantMemberships(self, peers, 1234); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("memberships %+v, want %+v", got, want)
	}
	if peers[1].Scopes[0].ExpiresAt != 0 {
		t.Fatal("the document's scope was changed in place")
	}
	if got := relayGrantMemberships(self, nil, 1234); got == nil || len(got) != 0 {
		t.Fatalf("an endpoint without peers gets %v, want an empty membership list (one grant per node)", got)
	}
}

// Session rights of the account's own peers stay out (the account network pairs them, P0 2026-10-26); standing
// rights of team members' CLIs (another account, no mesh) are carried once with the grant expiry (F5-10). 130 mesh
// CLIs with 4 session rights each and 64 members with 4 relay capabilities each make 2 + 4 memberships.
func TestRelayGrantMembershipsCarryStandingTeamScopesOnce(t *testing.T) {
	self := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "host"}}
	var peers []networkPeer
	for i := range 130 {
		scopes := []networkScope{
			{ResourceKind: "machine_network", ResourceID: fmt.Sprintf("network_%03d", i), ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443},
		}
		for _, capability := range []string{"terminal", "exec", "managed_ssh", "file_transfer"} {
			scopes = append(scopes, networkScope{ResourceKind: "machine_access", ResourceID: fmt.Sprintf("s_%03d", i), ResourceGeneration: 1, Capability: capability, Direction: "accept", Port: 443, ExpiresAt: 2_000})
		}
		peers = append(peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: fmt.Sprintf("cli_%03d", i), WireGuardPublicKey: fmt.Sprintf("key_%03d", i)}}, Scopes: scopes})
	}
	for i := range 64 {
		var scopes []networkScope
		for _, capability := range []string{"terminal", "exec", "managed_ssh", "file_transfer", "inspector"} {
			scopes = append(scopes, networkScope{ResourceKind: "machine_access", ResourceID: "host", ResourceGeneration: 7, Capability: capability, Direction: "accept", Port: 443})
		}
		peers = append(peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "other", EndpointID: fmt.Sprintf("member_%03d", i), WireGuardPublicKey: fmt.Sprintf("member_%03d", i)}}, Scopes: scopes})
	}
	got := relayGrantMemberships(self, peers, 1234)
	if len(got) != 5 || got[0].ResourceID != peernetwork.AccountNetworkID("account") {
		t.Fatalf("memberships %+v, want the account network (accept) and the 4 standing team capabilities", got)
	}
	for _, m := range got[1:] {
		if m.ResourceID != "host" || m.ExpiresAt != 1234 {
			t.Fatalf("membership %+v, want a standing team right with the grant expiry", m)
		}
	}
}

// Every membership reaches the grant, the account network first, however many there are (F21-164 once cut a busy
// host's longest-lived sessions at 4,096), and the parts take as many as the token bound allows.
func TestRelayGrantMembershipsKeepEveryMembershipPastTheOldCountCap(t *testing.T) {
	self := networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "host"}}
	const others = 4096 + 100
	var peers []networkPeer
	for i := range others {
		peers = append(peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "other", EndpointID: fmt.Sprintf("member_%04d", i), WireGuardPublicKey: fmt.Sprintf("key_%04d", i)}},
			Scopes: []networkScope{{ResourceKind: "machine_access", ResourceID: fmt.Sprintf("session_%04d", i), ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: int64(1_000 + i%50)}}})
	}
	peers = append(peers, networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: "account", EndpointID: "cli", WireGuardPublicKey: "key_cli"}}, Scopes: []networkScope{
		{ResourceKind: "machine_network", ResourceID: "network_cli", ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443}}})
	got := relayGrantMemberships(self, peers, 9_000)
	if len(got) != others+1 || got[0].ResourceKind != "machine_network" {
		t.Fatalf("%d memberships, first %+v: want all %d with the account network first", len(got), got[0], others+1)
	}
	kept, widest := 0, 0
	for _, part := range relayGrantMembershipParts(got) {
		kept += len(part)
		widest = max(widest, len(part))
	}
	if kept != len(got) || widest <= 256 {
		t.Fatalf("parts hold %d of %d memberships, widest %d: want all of them and a part past the old 256", kept, len(got), widest)
	}
}
