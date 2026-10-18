package peersessions

import (
	"fmt"
	"testing"
)

func TestBoundGrantsKeepsTheFirstEntriesInsteadOfFailing(t *testing.T) {
	few := make([]int, 10)
	if got := boundGrants(few); len(got) != 10 {
		t.Fatalf("a small set was changed: %d", len(got))
	}
	many := make([]int, 200)
	for i := range many {
		many[i] = i
	}
	got := boundGrants(many)
	if len(got) != maximumNetworkGrants || got[0] != 0 || got[len(got)-1] != maximumNetworkGrants-1 {
		t.Fatalf("bounded set = %d entries (first %d, last %d)", len(got), got[0], got[len(got)-1])
	}
}

func TestRelayControlPeersBoundTheGrantInsteadOfFailingTheRefresh(t *testing.T) {
	self := networkBinding{AccountID: "account", EndpointID: "self"}
	var peers []networkPeer
	for i := 0; i < 22; i++ {
		peers = append(peers, networkPeer{Identity: networkBinding{AccountID: "account", EndpointID: fmt.Sprintf("endpoint_%02d", i), WireGuardPublicKey: fmt.Sprintf("key_%02d", i)}})
	}
	got, err := relayControlPeers(self, peers)
	if err != nil || len(got) != maximumRelayControlPeers {
		t.Fatalf("22 same-account peers gave %d relay control peers, err=%v", len(got), err)
	}
}
