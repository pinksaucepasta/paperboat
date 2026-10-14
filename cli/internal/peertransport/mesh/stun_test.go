package mesh

import (
	"tailscale.com/tailcfg"
	"testing"
)

func TestSTUNConfigurationIsBoundedAndSeparateFromRelays(t *testing.T) {
	for _, s := range []string{"", "localhost:3478", "127.0.0.1:3478", "[::ffff:127.0.0.1]:3478", "10.0.0.1:3478", "[fe80::1%eth0]:3478", "stun.example.com:0", "stun.example.com:03478", "stun://stun.example.com:3478", "STUN.example.com:3478", "bad_.example.com:3478", "stun.example.com:65536"} {
		if ValidateSTUNServers([]string{s}) == nil {
			t.Errorf("accepted %q", s)
		}
	}
	if ValidateSTUNServers(make([]string, MaxSTUNServers+1)) == nil {
		t.Fatal("accepted unbounded discovery")
	}
	if ValidateSTUNServers([]string{"stun.example.com:3478", "stun.example.com:3478"}) == nil {
		t.Fatal("accepted duplicate discovery")
	}
	servers := []string{"stun.example.com:3478", "[2606:4700::1]:3478"}
	if err := ValidateSTUNServers(servers); err != nil {
		t.Fatal(err)
	}
	original := &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{65535: {RegionID: 65535, Nodes: []*tailcfg.DERPNode{{Name: "real", HostName: "relay.example.com"}}}}}
	combined := withSTUNServers(original, servers)
	if len(original.Regions) != 1 || len(combined.Regions) != 3 || combined.Regions[65535].Nodes[0].STUNOnly {
		t.Fatal("discovery changed relay inventory")
	}
	for i := range servers {
		n := combined.Regions[stunRegionBase+tailcfg.DERPRegionID(i)].Nodes[0]
		if !n.STUNOnly || n.DERPPort != 0 {
			t.Fatal("discovery gained relay capability")
		}
	}
	removed := withSTUNServers(combined, nil)
	if len(removed.Regions) != 1 || removed.Regions[65535] == nil {
		t.Fatal("withdrawal changed relay")
	}
}

func TestSTUNOnlyInventoryCannotBeAdvertisedAsRelayHome(t *testing.T) {
	b := &locoBackend{dm: withSTUNServers(&tailcfg.DERPMap{}, []string{"stun.example.com:3478"}), homeDERP: stunRegionBase}
	if got := b.derpRegionIDLocked(); got != 0 {
		t.Fatalf("discovery advertised as home: %d", got)
	}
	b.dm.Regions[3] = &tailcfg.DERPRegion{RegionID: 3, Nodes: []*tailcfg.DERPNode{{Name: "real"}}}
	if got := b.derpRegionIDLocked(); got != 3 {
		t.Fatalf("relay home=%d", got)
	}
}
