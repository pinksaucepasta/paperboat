package mesh

import (
	"testing"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-accounting-test
	"tailscale.com/ipn/ipnstate"
)

func TestUsagePathFromStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status *ipnstate.PeerStatus
		mode   string
		node   string
	}{
		{"absent", nil, "unknown", ""},
		{"idle", &ipnstate.PeerStatus{CurAddr: "192.0.2.1:1", Relay: "region"}, "unknown", ""},
		{"direct_with_fallback", &ipnstate.PeerStatus{Active: true, CurAddr: "192.0.2.1:1", Relay: "region"}, "direct", ""},
		{"peer_relay", &ipnstate.PeerStatus{Active: true, PeerRelay: "192.0.2.1:1:vni:123", Relay: "fallback"}, "relay", ""},
		{"regional", &ipnstate.PeerStatus{Active: true, Relay: "relay-node"}, "relay", ""},
		{"transition", &ipnstate.PeerStatus{Active: true}, "unknown", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := usagePathFromStatus(tc.status)
			if got.IsRegionalRelay() != (tc.name == "regional") {
				t.Fatalf("regional accounting discriminator incorrect for %s", tc.name)
			}
			if got.Mode != tc.mode || got.NodeID != tc.node {
				t.Fatalf("path = %#v, want mode %q without inferred node", got, tc.mode)
			}
		})
	}
}
