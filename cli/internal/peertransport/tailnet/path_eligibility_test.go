package tailnet

import (
	"context"
	"testing"
	"time"
)

// File transfer modes (PLAN 3b): Unlimited may use only direct paths and the account's own relays. A relay
// counts as the account's only when the carrying node is known to be self-hosted, or, when the carrier
// cannot be proven, when every relay the engine may use is.
func TestPathKindForTransferModes(t *testing.T) {
	own := RegionalNode{NodeID: "relay_own", Hosting: "self_hosted"}
	managed := RegionalNode{NodeID: "relay_managed"}
	both := map[int]RegionalNode{1: own, 2: managed}
	onlyOwn := map[int]RegionalNode{1: own}
	for name, c := range map[string]struct {
		path  UsagePath
		nodes map[int]RegionalNode
		want  PathKind
	}{
		"direct":                         {UsagePath{Mode: "direct"}, both, PathDirect},
		"idle":                           {UsagePath{Mode: "unknown"}, both, PathNone},
		"carried by the account's relay": {UsagePath{Mode: "relay", NodeID: "relay_own"}, both, PathSelfHostedRelay},
		"carried by a managed relay":     {UsagePath{Mode: "relay", NodeID: "relay_managed"}, both, PathHostedRelay},
		"carrier no longer listed":       {UsagePath{Mode: "relay", NodeID: "relay_gone"}, onlyOwn, PathHostedRelay},
		"unproven, mixed relays":         {UsagePath{Mode: "relay"}, both, PathHostedRelay},
		"unproven, only own relays":      {UsagePath{Mode: "relay"}, onlyOwn, PathSelfHostedRelay},
		"unproven, no relays":            {UsagePath{Mode: "relay"}, map[int]RegionalNode{}, PathHostedRelay},
	} {
		if got := classifyPath(c.path, c.nodes); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// The change channel ends with its context, so a transfer that stops watching leaves nothing running.
func TestPathEligibilityChannelEndsWithItsContext(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	ctx, cancel := context.WithCancel(t.Context())
	kind, changed := a.PathEligibility(ctx, "machine_test")
	if kind != PathNone {
		t.Fatalf("an engine without a path reported %s", kind)
	}
	cancel()
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("the change channel outlived its context")
	}
}
