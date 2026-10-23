package derpquic

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// The server issues one grant part per 64 peers, each with its own peers' peer-relay control keys; a
// fixed 16 per grant (and in total on the CLI) left a 44-96 machine account with only 16 control peers.
func TestGrantPartsMergeTheirRelayControlPeers(t *testing.T) {
	part := func(from, n int) Grant {
		g := Grant{Generation: 3}
		for i := from; i < from+n; i++ {
			g.RelayControlPeers = append(g.RelayControlPeers, fmt.Sprintf("control-%04d", i))
		}
		return g
	}
	merged, err := mergeGrants(part(0, MaxGrantPeers), part(MaxGrantPeers-4, MaxGrantPeers))
	if err != nil {
		t.Fatal(err)
	}
	if want := 2*MaxGrantPeers - 4; len(merged.RelayControlPeers) != want {
		t.Fatalf("merged %d control peers, want %d (duplicates once)", len(merged.RelayControlPeers), want)
	}
	if _, err := mergeGrants(part(0, MaxMergedPeers), part(MaxMergedPeers, 1)); !errors.Is(err, ErrAdmission) {
		t.Fatalf("merging past MaxMergedPeers control peers: %v", err)
	}
}

// An endpoint that names thousands of peers sends one part per 64: merging rebuilt the whole peer list
// and index for every part, so admitting all parts cost O(peers²/64). A part now costs its own size;
// a re-named peer takes the newer scopes, and a refused merge leaves the admitted grant unchanged.
func TestGrantPartsMergeInTheirOwnSize(t *testing.T) {
	part := func(from, n int, resource string) Grant {
		g := Grant{Generation: 3}
		for i := from; i < from+n; i++ {
			key := fmt.Sprintf("peer-%05d", i)
			g.Peers = append(g.Peers, Peer{WireGuardPublicKey: key, Scopes: []Scope{{ResourceID: resource}}})
			g.RelayControlPeers = append(g.RelayControlPeers, key)
		}
		return g.withIndex()
	}
	merged := part(0, MaxGrantPeers, "first")
	start := time.Now()
	for from := MaxGrantPeers; from < MaxMergedPeers; from += MaxGrantPeers {
		var err error
		if merged, err = mergeGrants(merged, part(from, MaxGrantPeers, "first")); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if len(merged.Peers) != MaxMergedPeers || len(merged.RelayControlPeers) != MaxMergedPeers {
		t.Fatalf("merged %d peers and %d control peers, want %d", len(merged.Peers), len(merged.RelayControlPeers), MaxMergedPeers)
	}
	merged, err := mergeGrants(merged, part(7, 2, "renewed"))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := merged.peerEntry("peer-00008"); !ok || p.Scopes[0].ResourceID != "renewed" || len(merged.Peers) != MaxMergedPeers {
		t.Fatalf("a re-named peer kept %+v (found %v, %d peers); want the newer scopes", p, ok, len(merged.Peers))
	}
	if _, err := mergeGrants(merged, part(MaxMergedPeers, 1, "over")); !errors.Is(err, ErrAdmission) {
		t.Fatalf("merging past MaxMergedPeers: %v", err)
	}
	if _, ok := merged.peerEntry(fmt.Sprintf("peer-%05d", MaxMergedPeers)); ok || len(merged.Peers) != MaxMergedPeers || merged.controls(fmt.Sprintf("peer-%05d", MaxMergedPeers)) {
		t.Fatal("a refused merge changed the admitted grant")
	}
	t.Logf("%d parts of %d merged in %v", MaxMergedPeers/MaxGrantPeers, MaxGrantPeers, elapsed)
}
