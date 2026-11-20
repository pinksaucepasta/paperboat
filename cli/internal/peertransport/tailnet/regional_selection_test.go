package tailnet

import (
	"testing"
	"time"
)

func selectionScore(id, domain string, rtt time.Duration) regionalScore {
	// Each test node is its own region, so these tests keep ranking nodes by RTT.
	return regionalScore{node: RegionalNode{NodeID: id, Region: id, FailureDomain: domain, CapacityLimit: 100}, rtt: rtt}
}

func TestRegionalSelectionSustainedGainAndCommit(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := selectionScore("a", "one", 100*time.Millisecond), selectionScore("b", "two", 70*time.Millisecond)
	var s regionalSelection
	chosen, _ := s.choose(now, []regionalScore{a})
	s.commit(chosen.node.NodeID, now)
	for _, offset := range []time.Duration{time.Second, 4 * time.Second, 9 * time.Second} {
		chosen, _ = s.choose(now.Add(offset), []regionalScore{a, b})
		if chosen.node.NodeID != "a" {
			t.Fatal("switched before sustained span")
		}
	}
	chosen, reason := s.choose(now.Add(12*time.Second), []regionalScore{a, b})
	if chosen.node.NodeID != "b" || reason != "sustained_improvement" || s.current() != "a" {
		t.Fatal("proposal changed committed route")
	}
	// Failed promotion does not commit, and the next fresh observation can retry.
	chosen, _ = s.choose(now.Add(15*time.Second), []regionalScore{a, b})
	if chosen.node.NodeID != "b" || s.current() != "a" {
		t.Fatal("lost pending improvement")
	}
	s.commit("b", now.Add(15*time.Second))
	if s.current() != "b" {
		t.Fatal("acknowledged promotion not committed")
	}
}

func TestRegionalSelectionFailureBypassesDwellAndRestorationStaysStable(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := selectionScore("a", "one", 80*time.Millisecond), selectionScore("b", "two", 90*time.Millisecond)
	var s regionalSelection
	s.choose(now, []regionalScore{a})
	s.commit("a", now)
	chosen, reason := s.choose(now.Add(time.Second), []regionalScore{b})
	if chosen.node.NodeID != "b" || reason != "selected_node_unreachable" {
		t.Fatal("failure waited for dwell")
	}
	s.commit("b", now.Add(time.Second))
	for _, offset := range []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second} {
		chosen, _ = s.choose(now.Add(offset), []regionalScore{a, b})
		if chosen.node.NodeID != "b" {
			t.Fatal("restored weak improvement caused flap")
		}
	}
}

func TestRegionalSelectionFreshSamplesAndJitter(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := selectionScore("a", "one", 100*time.Millisecond), selectionScore("b", "two", 60*time.Millisecond)
	var s regionalSelection
	s.choose(now, []regionalScore{a})
	s.commit("a", now)
	s.choose(now.Add(time.Second), []regionalScore{a, b})
	for _, offset := range []time.Duration{time.Second, 500 * time.Millisecond} {
		if chosen, reason := s.choose(now.Add(offset), []regionalScore{b}); chosen.node.NodeID != "" || reason != "stale_observation" {
			t.Fatal("stale input selected route")
		}
	}
	for i := 2; i < 10; i++ {
		candidate := b
		if i%2 == 0 {
			candidate.rtt = 95 * time.Millisecond
		}
		chosen, _ := s.choose(now.Add(time.Duration(i)*time.Second), []regionalScore{a, candidate})
		if chosen.node.NodeID != "a" {
			t.Fatal("jitter caused switch")
		}
	}
}

func TestRegionalSelectionIgnoresDeclaredCapacity(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := selectionScore("a", "one", 80*time.Millisecond), selectionScore("b", "two", 90*time.Millisecond)
	a.node.CapacityUsed = 100 // a full relay still ranks by RTT: declared capacity never ranks or refuses it
	scores := []regionalScore{b, a}
	rankRegional(scores)
	if scores[0].node.NodeID != "a" {
		t.Fatalf("rank order = %s first, want the lower-RTT relay", scores[0].node.NodeID)
	}
	var s regionalSelection
	s.choose(now, []regionalScore{a})
	s.commit("a", now)
	for _, offset := range []time.Duration{time.Second, 4 * time.Second, 11 * time.Second, 14 * time.Second} {
		if chosen, _ := s.choose(now.Add(offset), []regionalScore{a, b}); chosen.node.NodeID != "a" {
			t.Fatalf("switched to %s at %v; full capacity must not move a lower-RTT relay", chosen.node.NodeID, offset)
		}
	}
}

func TestRegionalSelectionInvalidInputAndBackupDomains(t *testing.T) {
	a, b, c := selectionScore("a", "one", 10*time.Millisecond), selectionScore("b", "one", 20*time.Millisecond), selectionScore("c", "two", 30*time.Millisecond)
	for _, tc := range []struct {
		scores     []regionalScore
		node       string
		redundancy Redundancy
	}{
		{[]regionalScore{a}, "", RedundancyReduced},
		{[]regionalScore{a, b}, "b", RedundancyReduced},
		{[]regionalScore{a, b, c}, "c", RedundancyAvailable},
		{nil, "", RedundancyNone},
	} {
		node, redundancy := regionalBackup(a.node, tc.scores)
		if node != tc.node || redundancy != tc.redundancy {
			t.Fatalf("backup=%s/%s", node, redundancy)
		}
	}
	bad := a
	bad.rtt = 0
	for _, scores := range [][]regionalScore{nil, {a, a}, {bad}, make([]regionalScore, MaxRegionalCandidates+1)} {
		var s regionalSelection
		if chosen, _ := s.choose(time.Now(), scores); chosen.node.NodeID != "" {
			t.Fatal("invalid scores accepted")
		}
		s.commit("a", time.Now())
		if s.current() != "" {
			t.Fatal("unoffered route committed")
		}
	}
}

func TestRegionalSelectionFollowsServerOrderWithinRegion(t *testing.T) {
	now := time.Unix(2000, 0)
	x := regionalScore{node: RegionalNode{NodeID: "x", Region: "r", FailureDomain: "one", CapacityLimit: 100}, rtt: 50 * time.Millisecond, order: 1}
	y := regionalScore{node: RegionalNode{NodeID: "y", Region: "r", FailureDomain: "two", CapacityLimit: 100}, rtt: 52 * time.Millisecond, order: 0}
	var s regionalSelection
	chosen, _ := s.choose(now, []regionalScore{x})
	s.commit(chosen.node.NodeID, now)
	// Same region: the server-first node is chosen at once, without the RTT hysteresis or dwell.
	chosen, reason := s.choose(now.Add(time.Second), []regionalScore{x, y})
	if chosen.node.NodeID != "y" || reason != "server_order_within_region" {
		t.Fatalf("chosen=%s reason=%s, want y server_order_within_region", chosen.node.NodeID, reason)
	}
	s.commit("y", now.Add(time.Second))
	if s.current() != "y" {
		t.Fatal("server-ordered promotion not committed")
	}
	// With y ineligible (failed or absent) the active x is kept.
	var kept regionalSelection
	chosen, _ = kept.choose(now, []regionalScore{x})
	kept.commit(chosen.node.NodeID, now)
	chosen, reason = kept.choose(now.Add(time.Second), []regionalScore{x})
	if chosen.node.NodeID != "x" || reason != "healthy_choice_retained" {
		t.Fatalf("chosen=%s reason=%s, want x healthy_choice_retained", chosen.node.NodeID, reason)
	}
}
