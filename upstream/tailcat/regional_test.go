package tailcat

import (
	"testing"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
)

func TestCloneRelayRegionsValidationAndOwnership(t *testing.T) {
	r := &tailcfg.DERPRegion{RegionID: 1, RegionCode: "one"}
	got, err := cloneRelayRegions([]*tailcfg.DERPRegion{r})
	if err != nil {
		t.Fatal(err)
	}
	r.RegionCode = "changed"
	if got[0].RegionCode != "one" {
		t.Fatal("relay region was not cloned")
	}
	for _, tc := range [][]*tailcfg.DERPRegion{
		{nil},
		{{RegionID: 0}},
		{{RegionID: 65536}},
		{{RegionID: 1}, {RegionID: 1}},
		make([]*tailcfg.DERPRegion, maxRelayRegions+1),
	} {
		if _, err := cloneRelayRegions(tc); err == nil {
			t.Fatalf("accepted invalid regions: %#v", tc)
		}
	}
	if got, err := cloneRelayRegions(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
}

func TestPeerDERPRegionUsesPromotedServerHome(t *testing.T) {
	server := key.NewNode().Public()
	b := &locoBackend{
		homeDERP: 1,
		dm: &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{
			1: {RegionID: 1}, 2: {RegionID: 2},
		}},
		nm: &netmap.NetworkMap{Peers: []tailcfg.NodeView{(&tailcfg.Node{Key: server, HomeDERP: 2}).View()}},
	}
	if got := b.peerDERPRegion(server); got != 2 {
		t.Fatalf("peerDERPRegion=%d, want promoted region 2", got)
	}
	if got := b.peerDERPRegion(key.NewNode().Public()); got != 1 {
		t.Fatalf("unknown peer region=%d, want startup region 1", got)
	}
}
