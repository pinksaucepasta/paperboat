package mesh

import (
	"testing"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=engine-unit-test
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=engine-unit-test
	"tailscale.com/types/key"
)

// A peer starting discovery needs our endpoints alone; advertising to every peer on each meow made
// bringing up N peers cost O(N²) sealed messages.
func TestEndpointTargetsAdvertiseToTheStartingPeerOnly(t *testing.T) {
	var peers []tailcfg.NodeView
	keys := make([]key.NodePublic, 5)
	for i := range keys {
		keys[i] = key.NewNode().Public()
		node := &tailcfg.Node{Key: keys[i], DiscoKey: key.NewDisco().Public()}
		if i%2 == 0 {
			node.HomeDERP = 7
		}
		peers = append(peers, node.View())
	}
	only := endpointTargets(peers, keys[1], 3)
	if len(only) != 1 || only[0].key != keys[1] || only[0].region != 3 {
		t.Fatalf("targets for one starting peer = %+v", only)
	}
	all := endpointTargets(peers, key.NodePublic{}, 3)
	if len(all) != 5 || all[0].region != 7 || all[1].region != 3 {
		t.Fatalf("broadcast targets = %+v", all)
	}
}
