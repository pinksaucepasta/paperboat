// Package edgeset assigns each workspace its edge pair: two regions (a primary and a backup) of the
// Paperboat-managed pool, or two regions of the workspace's own servers, never a mix. A region is a
// place users name ("Frankfurt"); which of its machines serve a workspace is the server's business,
// so replacing a machine never changes the pair. Placement consults the stored regions; they only
// change when a region is gone for good or the workspace changes its mode.
//
// Prior art: tailscale tailcfg.DERPRegion (tailcfg/derpmap.go) is a region with several Nodes the
// client does not choose between; headscale (hscontrol/derp/derp.go) merges regions of node lists.
package edgeset

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"time"
)

const (
	// Size is how many regions serve a workspace at once: the primary and the backup.
	Size = 2
	// ReplaceAfter is how long a region may be without a live node before it is replaced; a restart or a
	// short network cut keeps its place.
	ReplaceAfter = 10 * time.Minute
	// LatencyFresh is how old a machine's edge measurement may be and still place a workspace; older
	// samples are ignored. Machines repeat their samples on every attachment, so a day is generous.
	LatencyFresh = 24 * time.Hour
)

// choose keeps the regions of current that are still retained, then fills the remaining places from
// the usable regions: regions the workspace's machines measured come first, lowest median RTT first
// (rtt: region to milliseconds, empty when there is no fresh sample); the rest, and ties, go by highest
// rendezvous score, so a workspace without samples lands on a stable, evenly spread pair.
func choose(scope string, current []string, retained map[string]bool, rtt map[string]int, usable []string, size int) []string {
	chosen := make([]string, 0, size)
	taken := map[string]bool{}
	for _, region := range current {
		if retained[region] && !taken[region] && len(chosen) < size {
			chosen = append(chosen, region)
			taken[region] = true
		}
	}
	ranked := append([]string(nil), usable...)
	sort.Slice(ranked, func(i, j int) bool {
		ri, iok := rtt[ranked[i]]
		rj, jok := rtt[ranked[j]]
		if iok != jok {
			return iok
		}
		if iok && ri != rj {
			return ri < rj
		}
		si, sj := score(scope, ranked[i]), score(scope, ranked[j])
		if si != sj {
			return si > sj
		}
		return ranked[i] < ranked[j]
	})
	for _, region := range ranked {
		if len(chosen) >= size {
			break
		}
		if !taken[region] {
			chosen = append(chosen, region)
			taken[region] = true
		}
	}
	return chosen
}

func score(scope, region string) uint64 {
	digest := sha256.Sum256([]byte(scope + "\x00" + region))
	return binary.BigEndian.Uint64(digest[:8])
}
