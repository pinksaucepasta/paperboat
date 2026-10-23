package derpquic

import (
	"errors"
	"fmt"
	"testing"
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
