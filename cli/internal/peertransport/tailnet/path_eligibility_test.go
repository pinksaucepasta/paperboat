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

// An Unlimited transfer stops at the first WireGuard packet that crosses a hosted relay, not at the next
// status poll: magicsock sends both ways once its direct address stops being trusted, and a poll plus the
// engine's cached status let megabytes through (Lane H, F22-01, 10-19 MB per direct-path loss). Data over the
// account's own relay does not stop it.
func TestPathEligibilityEndsAtTheFirstHostedRelayPacket(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	peer := cfg.Peers[0].Identity
	public, err := publicKey(peer.WireGuardPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	a.relay.mu.Lock()
	a.relay.updateUsagePeersLocked(cfg.Peers)
	a.relay.mu.Unlock()
	own := &usageObservedCarrier{authority: &a.relay, node: RegionalNode{NodeID: "relay_own", Hosting: "self_hosted"}}
	hosted := &usageObservedCarrier{authority: &a.relay, node: RegionalNode{NodeID: "relay_managed"}}

	_, changed := a.PathEligibility(t.Context(), peer.EndpointID)
	own.record(public, false, time.Now())
	select {
	case <-changed:
		t.Fatal("data over the account's own relay ended an Unlimited transfer")
	case <-time.After(100 * time.Millisecond):
	}
	hosted.record(public, false, time.Now())
	select {
	case <-changed:
	case <-time.After(300 * time.Millisecond): // well inside the 500 ms status poll
		t.Fatal("the first hosted-relay packet did not end the watch")
	}
	a.relay.mu.Lock()
	recent := a.relay.hostedRelayRecentLocked(public, time.Now())
	stale := a.relay.hostedRelayRecentLocked(public, time.Now().Add(2*relayUsageFreshness))
	a.relay.mu.Unlock()
	if !recent || stale {
		t.Fatalf("hosted relay data is recent=%v, and stale after the freshness window=%v", recent, stale)
	}
}
