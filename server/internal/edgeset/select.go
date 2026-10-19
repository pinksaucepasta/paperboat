// Package edgeset assigns each workspace its set of edges: the Paperboat-managed pool or the
// workspace's own selected servers, never a mix. Placement consults the stored set; the set only
// changes when a member is gone for good or the workspace changes its mode.
package edgeset

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"time"
)

const (
	// Size is how many edges serve a workspace at once.
	Size = 2
	// ReplaceAfter is how long a member may be silent before it is replaced; a restart or a short
	// network cut keeps its place.
	ReplaceAfter = 10 * time.Minute
)

// Candidate is an edge that may serve a workspace right now.
type Candidate struct {
	NodeID        string
	FailureDomain string
}

// choose keeps the members of current that are still retained, then fills the remaining places
// from usable candidates: edges in failure domains the set does not use yet first, highest
// rendezvous score first, so each workspace lands on a stable, evenly spread pair.
func choose(scope string, current []string, retained map[string]bool, usable []Candidate, size int) []string {
	chosen := make([]string, 0, size)
	domains := map[string]bool{}
	taken := map[string]bool{}
	domainOf := map[string]string{}
	for _, candidate := range usable {
		domainOf[candidate.NodeID] = candidate.FailureDomain
	}
	for _, id := range current {
		if retained[id] && !taken[id] && len(chosen) < size {
			chosen = append(chosen, id)
			taken[id] = true
			if domain, ok := domainOf[id]; ok && domain != "" {
				domains[domain] = true
			}
		}
	}
	ranked := append([]Candidate(nil), usable...)
	sort.Slice(ranked, func(i, j int) bool {
		si, sj := score(scope, ranked[i].NodeID), score(scope, ranked[j].NodeID)
		if si != sj {
			return si > sj
		}
		return ranked[i].NodeID < ranked[j].NodeID
	})
	for _, distinct := range []bool{true, false} {
		for _, candidate := range ranked {
			if len(chosen) >= size {
				return chosen
			}
			if taken[candidate.NodeID] || distinct && candidate.FailureDomain != "" && domains[candidate.FailureDomain] {
				continue
			}
			chosen = append(chosen, candidate.NodeID)
			taken[candidate.NodeID] = true
			if candidate.FailureDomain != "" {
				domains[candidate.FailureDomain] = true
			}
		}
	}
	return chosen
}

func score(scope, node string) uint64 {
	digest := sha256.Sum256([]byte(scope + "\x00" + node))
	return binary.BigEndian.Uint64(digest[:8])
}
