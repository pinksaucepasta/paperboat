package mesh

import (
	"testing"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=engine-unit-test
	"tailscale.com/types/key"
)

// A machine's merged peer-relay control set covers every chunk of its grant, not 16 peers.
func TestRelayControlPeersCoverEveryGrantChunk(t *testing.T) {
	b := &locoBackend{pub: key.NewNode().Public()}
	peers := make([]key.NodePublic, 100)
	for i := range peers {
		peers[i] = key.NewNode().Public()
	}
	if err := b.setRelayControlPeers(peers); err != nil {
		t.Fatalf("100 control peers refused: %v", err)
	}
	if len(b.relayControlPeers) != 100 {
		t.Fatalf("kept %d control peers", len(b.relayControlPeers))
	}
}
