package tailnet

import (
	"testing"
	"time"
)

func selectionScore(id, domain string, rtt time.Duration) regionalScore {
	return regionalScore{node: RegionalNode{NodeID: id, FailureDomain: domain, CapacityLimit: 100}, rtt: rtt}
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

func TestRegionalSelectionUsesCapacityAdjustedQuality(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := selectionScore("a", "one", 80*time.Millisecond), selectionScore("b", "two", 90*time.Millisecond)
	a.node.CapacityUsed = 100 // Effective quality is 160 ms, despite smaller raw RTT.
	var s regionalSelection
	s.choose(now, []regionalScore{a})
	s.commit("a", now)
	for _, offset := range []time.Duration{time.Second, 4 * time.Second, 11 * time.Second} {
		s.choose(now.Add(offset), []regionalScore{a, b})
	}
	chosen, reason := s.choose(now.Add(14*time.Second), []regionalScore{a, b})
	if chosen.node.NodeID != "b" || reason != "sustained_improvement" {
		t.Fatal("ranking and switch quality disagree")
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
	bad.node.CapacityLimit = 0
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
