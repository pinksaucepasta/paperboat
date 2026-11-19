package peersessions

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/capver"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// The production claims function omits home_relay for a client whose capability version predates it, and keeps it
// for one that understands it (incident 2026-11-19: older clients rejected the whole document).
func TestConfigurationClaimsOmitHomeRelayForOlderClients(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	self, peer := contractBinding("usr_a", "machine_self", peernetwork.RoleMachine, 1), contractBinding("usr_a", "machine_peer", peernetwork.RoleMachine, 2)
	peer.Endpoints, peer.HomeRelay = []string{"203.0.113.7:41641"}, "relay_1"
	peers := []networkPeer{{Identity: peer, Scopes: []networkScope{personalNetworkScope(self, peer, now.Add(time.Minute).Unix())}}}
	for _, test := range []struct {
		capVersion int
		want       string
	}{{0, ""}, {1, ""}, {2, "relay_1"}, {capver.Version, "relay_1"}} {
		got := configurationClaims("https://api.example.test", now, now.Add(time.Minute), 1, self, peers, test.capVersion)
		if got.Peers[0].HomeRelay != test.want || len(got.Peers[0].Endpoints) != 1 {
			t.Errorf("capver %d: home_relay %q (want %q), endpoints %v", test.capVersion, got.Peers[0].HomeRelay, test.want, got.Peers[0].Endpoints)
		}
	}
	if peers[0].Identity.HomeRelay != "relay_1" {
		t.Fatal("the cached policy document was modified")
	}
}
