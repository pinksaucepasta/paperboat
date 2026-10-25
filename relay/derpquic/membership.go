package derpquic

import (
	"hash/maphash"
	"sort"
)

// Memberships let a grant authorize pairs without naming every peer (PLAN 3b "Relay pair authorization by
// membership"). A membership is a scope attached to the endpoint itself: the account's mesh network, one
// access grant, share or private route. Two admitted grants form a pair when they hold the same resource
// (kind, id, generation, capability, port) in opposite directions, exactly what per-peer scopes require,
// without one entry per peer: a mesh endpoint of a 10,000-machine account carries one membership instead
// of 10,000 peers. A grant uses memberships or per-peer lists, not both.

// MaxGrantMemberships bounds the memberships one grant carries.
const MaxGrantMemberships = 256

type membershipKey struct {
	kind, id, capability string
	generation           uint64
	port                 uint16
}

func keyOf(s Scope) membershipKey {
	return membershipKey{kind: s.ResourceKind, id: s.ResourceID, capability: s.Capability, generation: s.ResourceGeneration, port: s.Port}
}

// validScope is the shape every grant scope, per-peer or membership, must have.
func validScope(s Scope) bool {
	validResource := s.ResourceKind == "config_compare" && s.Capability == "config_compare" || s.ResourceKind == "machine_network" && s.Capability == "connect" || s.ResourceKind == "inspector" && s.Capability == "inspector" || s.ResourceKind == "machine_access" && (s.Capability == "terminal" || s.Capability == "exec" || s.Capability == "managed_ssh" || s.Capability == "file_transfer" || s.Capability == "private_access") || s.ResourceKind == "codex_session" && s.Capability == "codex"
	return validResource && s.ResourceID != "" && len(s.ResourceID) <= 256 && s.ResourceGeneration != 0 && s.Port == 443 && (s.Direction == "dial" || s.Direction == "accept") && s.ExpiresAt > 0
}

// validMemberships checks a grant's memberships: valid scopes, bounded, no duplicate, and no per-peer list
// beside them.
func validMemberships(g Grant) bool {
	if len(g.Memberships) == 0 {
		return true
	}
	if len(g.Memberships) > MaxGrantMemberships || len(g.Peers) != 0 || len(g.RelayControlPeers) != 0 {
		return false
	}
	seen := make(map[Scope]bool, len(g.Memberships))
	for _, m := range g.Memberships {
		if !validScope(m) || seen[m] {
			return false
		}
		seen[m] = true
	}
	return true
}

// membershipIndex is a grant's memberships ordered by the hash of their resource key: 12 bytes per
// membership and a binary search per lookup. A map keyed by the three-string resource cost ~1.6 KiB per mesh
// grant and ~570 ns per pair check at 10,000 endpoints (TestTenThousandMeshEndpointsFitTheMembershipBudget).
type membershipIndex struct {
	hashes []uint64 // sorted
	order  []int32  // order[i] is the membership whose key hashes to hashes[i]
}

var membershipSeed = maphash.MakeSeed()

func newMembershipIndex(memberships []Scope) membershipIndex {
	if len(memberships) == 0 {
		return membershipIndex{}
	}
	index := membershipIndex{hashes: make([]uint64, len(memberships)), order: make([]int32, len(memberships))}
	for i, m := range memberships {
		index.hashes[i], index.order[i] = maphash.Comparable(membershipSeed, keyOf(m)), int32(i)
	}
	sort.Sort(byHash(index))
	return index
}

type byHash membershipIndex

func (b byHash) Len() int           { return len(b.hashes) }
func (b byHash) Less(i, j int) bool { return b.hashes[i] < b.hashes[j] }
func (b byHash) Swap(i, j int) {
	b.hashes[i], b.hashes[j] = b.hashes[j], b.hashes[i]
	b.order[i], b.order[j] = b.order[j], b.order[i]
}

// first is the position of the first entry hashing to h.
func (ix *membershipIndex) first(h uint64) int {
	lo, hi := 0, len(ix.hashes)
	for lo < hi {
		mid := (lo + hi) / 2
		if ix.hashes[mid] < h {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// reciprocalMemberships calls match for every reciprocal membership pair of a and b: one binary search in b
// per distinct resource of a. Keys are compared, so a hash collision cannot pair different resources.
func reciprocalMemberships(a, b Grant, match func(Scope, Scope)) {
	if a.index == nil {
		a = a.withIndex()
	}
	if b.index == nil {
		b = b.withIndex()
	}
	sameAccount := a.AccountID == b.AccountID
	ai, bi := &a.index.memberships, &b.index.memberships
	for i := 0; i < len(ai.hashes); i++ {
		h := ai.hashes[i]
		start := bi.first(h)
		for ; i < len(ai.hashes) && ai.hashes[i] == h; i++ {
			s := &a.Memberships[ai.order[i]]
			if s.ResourceKind == "machine_network" && !sameAccount {
				continue
			}
			for j := start; j < len(bi.hashes) && bi.hashes[j] == h; j++ {
				t := &b.Memberships[bi.order[j]]
				if s.Direction != t.Direction && keyOf(*s) == keyOf(*t) {
					match(*s, *t)
				}
			}
		}
		i--
	}
}

// eachMembership calls f with every membership of g whose key hashes to h.
func (g *Grant) eachMembership(h uint64, f func(Scope)) {
	ix := &g.index.memberships
	for i := ix.first(h); i < len(ix.hashes) && ix.hashes[i] == h; i++ {
		f(g.Memberships[ix.order[i]])
	}
}

// networkMember reports whether g holds its own account's mesh membership, which admits relay-control
// traffic between endpoints of that account.
func (g Grant) networkMember() bool {
	for _, m := range g.Memberships {
		if m.ResourceKind == "machine_network" {
			return true
		}
	}
	return false
}

// mergeMemberships is the union of two parts' memberships, newest expiry kept.
func mergeMemberships(current, next []Scope) []Scope {
	out := append([]Scope(nil), current...)
	at := make(map[Scope]int, len(out))
	for i, m := range out {
		n := m
		n.ExpiresAt = 0
		at[n] = i
	}
	for _, m := range next {
		n := m
		n.ExpiresAt = 0
		if i, ok := at[n]; ok {
			out[i].ExpiresAt = max(out[i].ExpiresAt, m.ExpiresAt)
			continue
		}
		at[n] = len(out)
		out = append(out, m)
	}
	return out
}
