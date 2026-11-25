package tailnet

import (
	"testing"
	"time"
)

func selectionScore(id, domain string, rtt time.Duration) regionalScore {
	// Each test node is its own region, so these tests keep ranking nodes by RTT.
	return regionalScore{node: RegionalNode{NodeID: id, Region: id, FailureDomain: domain}, rtt: rtt}
}

func TestRegionalSelectionSustainedGainAndCommit(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := selectionScore("a", "one", 100*time.Millisecond), selectionScore("b", "two", 70*time.Millisecond)
	var s regionalSelection
	chosen, _ := s.choose(now, []regionalScore{a}, false)
	s.commit(chosen.node.NodeID, now)
	for _, offset := range []time.Duration{time.Second, 4 * time.Second, 9 * time.Second} {
		chosen, _ = s.choose(now.Add(offset), []regionalScore{a, b}, false)
		if chosen.node.NodeID != "a" {
			t.Fatal("switched before sustained span")
		}
	}
	chosen, reason := s.choose(now.Add(12*time.Second), []regionalScore{a, b}, false)
	if chosen.node.NodeID != "b" || reason != "sustained_improvement" || s.current() != "a" {
		t.Fatal("proposal changed committed route")
	}
	// Failed promotion does not commit, and the next fresh observation can retry.
	chosen, _ = s.choose(now.Add(15*time.Second), []regionalScore{a, b}, false)
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
	s.choose(now, []regionalScore{a}, false)
	s.commit("a", now)
	chosen, reason := s.choose(now.Add(time.Second), []regionalScore{b}, false)
	if chosen.node.NodeID != "b" || reason != "selected_node_unreachable" {
		t.Fatal("failure waited for dwell")
	}
	s.commit("b", now.Add(time.Second))
	for _, offset := range []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second} {
		chosen, _ = s.choose(now.Add(offset), []regionalScore{a, b}, false)
		if chosen.node.NodeID != "b" {
			t.Fatal("restored weak improvement caused flap")
		}
	}
}

func TestRegionalSelectionFreshSamplesAndJitter(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := selectionScore("a", "one", 100*time.Millisecond), selectionScore("b", "two", 60*time.Millisecond)
	var s regionalSelection
	s.choose(now, []regionalScore{a}, false)
	s.commit("a", now)
	s.choose(now.Add(time.Second), []regionalScore{a, b}, false)
	for _, offset := range []time.Duration{time.Second, 500 * time.Millisecond} {
		if chosen, reason := s.choose(now.Add(offset), []regionalScore{b}, false); chosen.node.NodeID != "" || reason != "stale_observation" {
			t.Fatal("stale input selected route")
		}
	}
	for i := 2; i < 10; i++ {
		candidate := b
		if i%2 == 0 {
			candidate.rtt = 95 * time.Millisecond
		}
		chosen, _ := s.choose(now.Add(time.Duration(i)*time.Second), []regionalScore{a, candidate}, false)
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
	s.choose(now, []regionalScore{a}, false)
	s.commit("a", now)
	for _, offset := range []time.Duration{time.Second, 4 * time.Second, 11 * time.Second, 14 * time.Second} {
		if chosen, _ := s.choose(now.Add(offset), []regionalScore{a, b}, false); chosen.node.NodeID != "a" {
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
		if chosen, _ := s.choose(time.Now(), scores, false); chosen.node.NodeID != "" {
			t.Fatal("invalid scores accepted")
		}
		s.commit("a", time.Now())
		if s.current() != "" {
			t.Fatal("unoffered route committed")
		}
	}
}

func TestRegionalSelectionKeepsActiveNodeWithinRegion(t *testing.T) {
	now := time.Unix(2000, 0)
	x := regionalScore{node: RegionalNode{NodeID: "x", Region: "r", FailureDomain: "one"}, rtt: 50 * time.Millisecond, order: 1}
	y := regionalScore{node: RegionalNode{NodeID: "y", Region: "r", FailureDomain: "two"}, rtt: 30 * time.Millisecond, order: 0}
	var s regionalSelection
	chosen, _ := s.choose(now, []regionalScore{x}, false)
	s.commit(chosen.node.NodeID, now)
	// Same region: the active x stays although the server now lists y first, for as long as x is listed.
	for i := 1; i <= 5; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		chosen, reason := s.choose(at, []regionalScore{x, y}, false)
		if chosen.node.NodeID != "x" || reason != "healthy_choice_retained" {
			t.Fatalf("round %d chosen=%s reason=%s, want x healthy_choice_retained", i, chosen.node.NodeID, reason)
		}
		s.commit(chosen.node.NodeID, at)
	}
	// x removed: the server's first node of the region is taken at once.
	chosen, reason := s.choose(now.Add(time.Hour), []regionalScore{y}, false)
	if chosen.node.NodeID != "y" || reason != "selected_node_unreachable" {
		t.Fatalf("chosen=%s reason=%s, want y selected_node_unreachable", chosen.node.NodeID, reason)
	}
}

func TestRegionalSelectionKeepsARelayThatMissedOneSweep(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b := selectionScore("a", "one", 80*time.Millisecond), selectionScore("b", "two", 120*time.Millisecond)
	selected := func() *regionalSelection {
		s := &regionalSelection{}
		s.choose(now, []regionalScore{a}, false)
		s.commit("a", now)
		return s
	}
	t.Run("one missed sweep, also one that timed out past the window", func(t *testing.T) {
		for _, miss := range []time.Duration{3 * time.Second, 9 * time.Second} {
			s := selected()
			if chosen, reason := s.choose(now.Add(miss), []regionalScore{b}, true); chosen.node.NodeID != "" || reason != "selected_node_missed" {
				t.Fatalf("missed at +%v: chose %q (%s), want the selected relay kept", miss, chosen.node.NodeID, reason)
			}
			chosen, _ := s.choose(now.Add(miss+3*time.Second), []regionalScore{a, b}, true)
			if chosen.node.NodeID != "a" || s.current() != "a" || s.misses != 0 {
				t.Fatalf("after it answered again: chose %q, misses %d", chosen.node.NodeID, s.misses)
			}
		}
	})
	t.Run("a dead relay is left within 8 s of its first miss", func(t *testing.T) {
		// Sweeps after a miss run about 1 s apart (1.25 s with jitter) and probe for at most regionalMissedProbeTimeout.
		for _, first := range []time.Duration{3 * time.Second, 9 * time.Second} {
			s := selected()
			for at := first; ; at += 1250*time.Millisecond + regionalMissedProbeTimeout {
				chosen, reason := s.choose(now.Add(at), []regionalScore{b}, true)
				if chosen.node.NodeID == "b" {
					if at == first || reason != "selected_node_unreachable" || at-first > 8*time.Second {
						t.Fatalf("left %v after the first miss (%s)", at-first, reason)
					}
					break
				}
				if at-first > 8*time.Second {
					t.Fatal("a dead relay was kept for more than 8 s")
				}
			}
		}
	})
	t.Run("a refused, delisted or other peer's relay is left at once", func(t *testing.T) {
		s := selected()
		if chosen, reason := s.choose(now.Add(3*time.Second), []regionalScore{b}, false); chosen.node.NodeID != "b" || reason != "selected_node_unreachable" {
			t.Fatalf("chose %q (%s)", chosen.node.NodeID, reason)
		}
	})
}
