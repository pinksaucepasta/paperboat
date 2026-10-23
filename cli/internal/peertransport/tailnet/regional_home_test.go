package tailnet

import "testing"

// The engine advertises its first relay region as home. Nodes arrive sorted by region name, so the
// advertised home was the alphabetically first relay, not the one regional recovery selected, and
// peers sent their first packets to a relay this machine had not chosen (Lane X #14).
func TestAdvertisedHomeIsTheSelectedRelay(t *testing.T) {
	nodes := []RegionalNode{{NodeID: "relay-a", Region: "ap-1"}, {NodeID: "relay-b", Region: "ap-2"}}
	regions := relayRegionsSelectedFirst(nodes, "relay-b")
	if len(regions) != 2 || regions[0].RegionID != regionalID("relay-b") || regions[1].RegionID != regionalID("relay-a") {
		t.Fatalf("regions = %v, want relay-b first then relay-a", []int{int(regions[0].RegionID), int(regions[1].RegionID)})
	}
	if regions = relayRegionsSelectedFirst(nodes, ""); regions[0].RegionID != regionalID("relay-a") || len(regions) != 2 {
		t.Fatal("without a selection the region order changed or a region was dropped")
	}
}
