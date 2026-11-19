package peersessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// TestEmitCompatNetworkConfiguration writes, to $COMPAT_ARTIFACTS_DIR, the network configuration claims a client that
// sent no capability version receives: a peer with a home relay, built by the production claims function, must not
// carry it (peernetwork minCapVersion).
func TestEmitCompatNetworkConfiguration(t *testing.T) {
	dir := os.Getenv("COMPAT_ARTIFACTS_DIR")
	if dir == "" {
		t.Skip("set COMPAT_ARTIFACTS_DIR to emit compat artifacts for tools/compat")
	}
	now := time.Now().UTC().Truncate(time.Second)
	self, peer := contractBinding("usr_compat", "machine_self", peernetwork.RoleMachine, 1), contractBinding("usr_compat", "machine_peer", peernetwork.RoleMachine, 2)
	peer.Endpoints, peer.HomeRelay = []string{"203.0.113.7:41641"}, "relay_compat"
	expiry := now.Add(5 * time.Minute)
	peers := []networkPeer{{Identity: peer, Scopes: []networkScope{personalNetworkScope(self, peer, expiry.Unix())}}}
	raw, err := json.MarshalIndent(configurationClaims("https://api.compat.example.test", now, expiry, 1, self, peers, 0), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network_configuration.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
