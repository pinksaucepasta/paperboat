package tailnet

import (
	"testing"
	"time"
)

// regionScore builds a candidate in region with its position in the server's list.
func regionScore(id, region string, rtt time.Duration, order int) regionalScore {
	return regionalScore{node: RegionalNode{NodeID: id, Region: region, FailureDomain: id, CapacityLimit: 100}, rtt: rtt, order: order}
}

func TestRelayChoiceUsesTheServersFirstNodeWithinARegion(t *testing.T) {
	now := time.Unix(1000, 0)
	a := regionScore("a", "near", 40*time.Millisecond, 0)
	b := regionScore("b", "near", 10*time.Millisecond, 1) // lower RTT, but the server lists a first
	var s regionalSelection
	chosen, reason := s.choose(now, []regionalScore{b, a})
	if chosen.node.NodeID != "a" || reason != "initial_common_node" {
		t.Fatalf("chose %s (%s), want the server's first node a", chosen.node.NodeID, reason)
	}
}

func TestRelayChoicePicksTheNearestRegion(t *testing.T) {
	now := time.Unix(1000, 0)
	far := regionScore("far", "west", 80*time.Millisecond, 0) // server lists the far region first
	near := regionScore("near", "east", 20*time.Millisecond, 1)
	var s regionalSelection
	chosen, _ := s.choose(now, []regionalScore{far, near})
	if chosen.node.NodeID != "near" {
		t.Fatalf("chose %s, want the node of the region with the lower best RTT", chosen.node.NodeID)
	}
}

func TestRelayChoiceSkipsAFailedNodeWithinItsRegionFirst(t *testing.T) {
	now := time.Unix(1000, 0)
	first := regionScore("first", "near", 10*time.Millisecond, 0)
	second := regionScore("second", "near", 15*time.Millisecond, 1)
	farNode := regionScore("far", "west", 80*time.Millisecond, 0)
	var s regionalSelection
	chosen, _ := s.choose(now, []regionalScore{first, second, farNode})
	s.commit(chosen.node.NodeID, now)
	if chosen.node.NodeID != "first" {
		t.Fatalf("initial choice %s, want first", chosen.node.NodeID)
	}
	// first fails: its region's second node is used, not the farther region.
	chosen, reason := s.choose(now.Add(time.Second), []regionalScore{second, farNode})
	if chosen.node.NodeID != "second" || reason != "selected_node_unreachable" {
		t.Fatalf("after failure chose %s (%s), want second in the same region", chosen.node.NodeID, reason)
	}
}
