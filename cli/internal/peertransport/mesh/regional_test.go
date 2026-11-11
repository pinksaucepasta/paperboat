package mesh

import (
	"testing"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
	"tailscale.com/wgengine/filter"
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
			1: {RegionID: 1, Nodes: []*tailcfg.DERPNode{{Name: "one"}}}, 2: {RegionID: 2, Nodes: []*tailcfg.DERPNode{{Name: "two"}}},
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

func TestEmptyRelayInventoryBeforeStartClearsBootstrap(t *testing.T) {
	s := new(Server)
	if err := s.SetRelayRegions([]*tailcfg.DERPRegion{{RegionID: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRelayRegions(nil); err != nil {
		t.Fatal(err)
	}
	if s.Region != nil || len(s.relayRegions) != 0 {
		t.Fatal("withdrawn bootstrap relay retained")
	}
}

func TestRedefinedPinnedRegionsReconnectsOnlyMovedPinnedRelays(t *testing.T) {
	region := func(id tailcfg.DERPRegionID, port int) *tailcfg.DERPRegion {
		return &tailcfg.DERPRegion{RegionID: id, Nodes: []*tailcfg.DERPNode{{Name: "n", RegionID: id, HostName: "127.0.0.1", DERPPort: port}}}
	}
	old := derpMapOf([]*tailcfg.DERPRegion{region(1, 100), region(2, 200), region(3, 300)})
	next := derpMapOf([]*tailcfg.DERPRegion{region(1, 101), region(2, 200), region(4, 400)})
	clients := map[key.NodePublic]*tailcfg.Node{}
	for _, home := range []tailcfg.DERPRegionID{1, 1, 2, 3, 4, 0} {
		clients[key.NewNode().Public()] = &tailcfg.Node{HomeDERP: home}
	}
	// 1 moved (once, though two peers use it); 2 is unchanged; 3 was withdrawn; 4 and 0 are new or unpinned.
	if got := redefinedPinnedRegions(old, next, clients); len(got) != 1 || got[0] != 1 {
		t.Fatalf("redefined pinned regions=%v, want [1]", got)
	}
	if got := redefinedPinnedRegions(nil, next, clients); len(got) != 0 {
		t.Fatalf("first inventory reconnected %v", got)
	}
}

// Repeated authority observations must not publish the same relay map or rebuild
// the filter. No engine is needed when both inventories are unchanged.
func TestUnchangedRelayInventoryDoesNotPublish(t *testing.T) {
	region := &tailcfg.DERPRegion{RegionID: 1, RegionCode: "one"}
	b := &locoBackend{homeDERP: 1, dm: derpMapOf([]*tailcfg.DERPRegion{region})}
	for range 100 {
		b.setRelayRegions([]*tailcfg.DERPRegion{region.Clone()})
		if err := b.replacePeerRelayNodes(nil, func() *filter.Filter {
			t.Fatal("unchanged services rebuilt filter")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
