package tailnet

import (
	"slices"
	"time"
)

// regionalSelection records only acknowledged routes. Observations can propose a
// replacement without making a failed promotion the next round's current route.
type regionalSelection struct {
	selected    string
	selectedAt  time.Time
	observedAt  time.Time
	candidate   string
	first, last time.Time
	samples     uint8
	offered     string
}

func (s *regionalSelection) current() string { return s.selected }

func (s *regionalSelection) clearCandidate() {
	s.candidate = ""
	s.first, s.last = time.Time{}, time.Time{}
	s.samples = 0
}

func validRegionalScores(scores []regionalScore) bool {
	if len(scores) == 0 || len(scores) > MaxRegionalCandidates {
		return false
	}
	seen := make(map[string]bool, len(scores))
	for _, score := range scores {
		n := score.node
		if n.NodeID == "" || len(n.NodeID) > 256 || seen[n.NodeID] || score.rtt <= 0 || score.rtt > time.Minute || n.CapacityLimit == 0 || n.CapacityUsed > n.CapacityLimit {
			return false
		}
		seen[n.NodeID] = true
	}
	return true
}

// regionalQuality matches rankRegional's fresh-capacity queueing penalty.
func regionalQuality(score regionalScore) float64 {
	return float64(score.rtt) * (1 + float64(score.node.CapacityUsed)/float64(score.node.CapacityLimit))
}

func (s *regionalSelection) choose(now time.Time, scores []regionalScore) (regionalScore, string) {
	s.offered = ""
	if now.IsZero() || !validRegionalScores(scores) {
		s.clearCandidate()
		return regionalScore{}, "no_mutually_reachable_node"
	}
	if !s.observedAt.IsZero() && !now.After(s.observedAt) {
		return regionalScore{}, "stale_observation"
	}
	s.observedAt = now
	ranked := slices.Clone(scores)
	rankRegional(ranked)
	best := ranked[0]
	var active regionalScore
	for _, score := range ranked {
		if score.node.NodeID == s.selected {
			active = score
			break
		}
	}
	if active.node.NodeID == "" {
		s.clearCandidate()
		s.offered = best.node.NodeID
		if s.selected == "" {
			return best, "initial_common_node"
		}
		return best, "selected_node_unreachable"
	}
	s.offered = active.node.NodeID
	currentQuality, bestQuality := regionalQuality(active), regionalQuality(best)
	gain := currentQuality - bestQuality
	if best.node.NodeID == s.selected || gain < float64(10*time.Millisecond) || gain < currentQuality*0.15 {
		s.clearCandidate()
		return active, "healthy_choice_retained"
	}
	if s.candidate != best.node.NodeID {
		s.candidate, s.first, s.last, s.samples = best.node.NodeID, now, now, 1
	} else if now.Sub(s.last) >= regionalProbeInterval {
		s.last = now
		if s.samples < 3 {
			s.samples++
		}
	}
	if now.Sub(s.selectedAt) < regionalDwell || s.samples < 3 || s.last.Sub(s.first) < regionalDwell {
		return active, "healthy_choice_retained"
	}
	s.offered = best.node.NodeID
	return best, "sustained_improvement"
}

func (s *regionalSelection) commit(node string, now time.Time) {
	if node == "" || node != s.offered || now.IsZero() || now.Before(s.observedAt) {
		return
	}
	if s.selected != node {
		s.selected, s.selectedAt = node, now
		s.clearCandidate()
	}
}

// regionalBackup returns the best measured alternate, preferring an independent
// failure domain. One healthy node is usable but has reduced redundancy.
func regionalBackup(chosen RegionalNode, scores []regionalScore) (string, Redundancy) {
	if !validRegionalScores(scores) || chosen.NodeID == "" {
		return "", RedundancyNone
	}
	ranked := slices.Clone(scores)
	rankRegional(ranked)
	present := false
	for _, score := range ranked {
		if score.node.NodeID == chosen.NodeID {
			present = true
			chosen = score.node
			break
		}
	}
	if !present {
		return "", RedundancyNone
	}
	backup := ""
	for _, score := range ranked {
		if score.node.NodeID == chosen.NodeID {
			continue
		}
		if backup == "" {
			backup = score.node.NodeID
		}
		if chosen.FailureDomain != "" && score.node.FailureDomain != "" && chosen.FailureDomain != score.node.FailureDomain {
			return score.node.NodeID, RedundancyAvailable
		}
	}
	return backup, RedundancyReduced
}
