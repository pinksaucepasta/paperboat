package peernetwork

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

// scopeCapabilities lists, for each resource kind of a signed access scope, the capabilities it may carry. It is the
// one list: the client accepts exactly these in a network configuration and the relay in a grant (anything else
// makes the whole document or grant invalid), and the server's tests check every scope it can emit against them.
// A (machine_access, inspector) rule the client did not know made hosts reject every renewal for three minutes on
// 2026-10-27; a (recording, recording) scope the relay did not know refused every relayed pair of the account on
// 2026-11-11.
var scopeCapabilities = map[string][]string{
	"machine_network": {"connect"}, // the account mesh pair; the client also requires the peer to be its account
	"machine_access":  {"terminal", "exec", "managed_ssh", "file_transfer", "private_access", "inspector"},
	"inspector":       {"inspector"},
	"config_compare":  {"config_compare"},
	"recording":       {"recording"}, // a CLI session recording replay (peerconsumer.Recording)
}

// NetworkPort is the one virtual port every scope names (the peer network serves only it).
const NetworkPort = 443

// MaxResourceIDBytes bounds a scope's resource id.
const MaxResourceIDBytes = 256

// Scope is one signed access scope: a peer entry's in a network configuration, and a peer's or membership's in a
// relay grant. The server builds it, the client and the relay verify it; all three use this type.
type Scope struct {
	ResourceKind       string `json:"resource_kind"`
	ResourceID         string `json:"resource_id"`
	ResourceGeneration uint64 `json:"resource_generation"`
	Capability         string `json:"capability"`
	Direction          string `json:"direction"`
	Port               uint16 `json:"port"`
	ExpiresAt          int64  `json:"expires_at"`
}

// ValidGrantScope is the relay's rule for a grant membership: a known kind and capability,
// a resource id, a generation, NetworkPort, a direction and an expiry (a grant stamps a standing scope with its own,
// RelayGrantKeepsStanding). The relay refuses a whole grant holding any other scope, so the server's mint and its
// contract tests check every scope it puts in a grant with this.
func ValidGrantScope(s Scope) bool {
	return ValidScope(s.ResourceKind, s.Capability) && s.ResourceID != "" && len(s.ResourceID) <= MaxResourceIDBytes &&
		s.ResourceGeneration != 0 && s.Port == NetworkPort && (s.Direction == "dial" || s.Direction == "accept") && s.ExpiresAt > 0
}

// ValidKind reports whether kind is a known scope resource kind.
func ValidKind(kind string) bool {
	_, ok := scopeCapabilities[kind]
	return ok
}

// ValidScope reports whether a signed access scope of kind may carry capability.
func ValidScope(kind, capability string) bool {
	return slices.Contains(scopeCapabilities[kind], capability)
}

// Scopes yields every valid (kind, capability), for the consumers' contract tests.
func Scopes(yield func(kind, capability string) bool) {
	for kind, capabilities := range scopeCapabilities {
		for _, c := range capabilities {
			if !yield(kind, c) {
				return
			}
		}
	}
}

// RelayGrantKeepsStanding reports whether a relay grant carries a scope held until revoked (expires_at 0); the grant
// stamps it with its own expiry, because relays refuse a scope without one. The server builds the grant and the CLI
// rebuilds the same set from its configuration, so both call this. A machine_network scope is kept. A machine_access
// scope is a session right that authorizes a relay pair only when no machine_network scope pairs the two endpoints
// (same account); it is kept for a peer without one, a team member's CLI and the shared host, whose pair the relay
// admits from reciprocal machine_access dial and accept scopes (derpquic reciprocalScopes). Keeping it for mesh peers
// as well would push 64 CLI peers x 4 capabilities past the relay's 128-scope grant bound (P0 2026-10-26).
func RelayGrantKeepsStanding(s Scope, peerHasMesh bool) bool {
	switch s.ResourceKind {
	case "machine_network":
		return true
	case "machine_access":
		return !peerHasMesh && s.ResourceGeneration >= 1 && s.Port == NetworkPort && (s.Direction == "dial" || s.Direction == "accept") &&
			(s.Capability == "terminal" || s.Capability == "exec" || s.Capability == "managed_ssh" || s.Capability == "file_transfer")
	}
	return false
}

// RelayGrantScopes is the part of one peer's document scopes a relay grant carries: every scope with an expiry, and
// of the until-revoked ones those RelayGrantKeepsStanding keeps, stamped with grantExpiry. The server builds the grant
// and the CLI rebuilds the same set, so both call this.
//
// One scope is one right of one access session (a terminal, an exec, ...) for one capability and direction. The relay
// uses scopes for one thing: a pair of endpoints may talk while a scope of one grant has a reciprocal scope (same
// resource, capability and generation, opposite direction) in the other's, live until min(both expiries). Which session
// carries the pair does not matter, only the latest expiry does. A grant holds at most MaxGrantScopes scopes and the
// relay refuses the whole grant over that, while a document names every session of the peer (100 terminals of three
// scopes each, F21-164). So over the bound the sessions of a (kind, capability, direction) are reduced to the
// relayGrantSessionsPerKey with the latest expiry (lowest id on a tie): both ends of a pair choose from the same set by
// the same rule, so they keep the same sessions, and the longest-lived reciprocal pair, the pair's lifetime, stays. A
// cut in document order (sorted by session id) would drop it arbitrarily. Scopes that were until-revoked (standing),
// the mesh pair among them, are never reduced.
func RelayGrantScopes(scopes []Scope, grantExpiry int64) []Scope {
	mesh := slices.ContainsFunc(scopes, func(s Scope) bool { return s.ResourceKind == "machine_network" })
	out := make([]Scope, 0, len(scopes))
	standing := make([]bool, 0, len(scopes))
	for _, scope := range scopes {
		kept := scope.ExpiresAt == 0
		if kept {
			if !RelayGrantKeepsStanding(scope, mesh) {
				continue
			}
			scope.ExpiresAt = grantExpiry
		}
		out = append(out, scope)
		standing = append(standing, kept)
	}
	if len(out) <= MaxGrantScopes {
		return out
	}
	type key struct{ kind, capability, direction string }
	groups := map[key][]int{}
	for i, scope := range out {
		if !standing[i] {
			k := key{scope.ResourceKind, scope.Capability, scope.Direction}
			groups[k] = append(groups[k], i)
		}
	}
	drop := make([]bool, len(out))
	for _, indexes := range groups {
		slices.SortFunc(indexes, func(a, b int) int {
			if out[a].ExpiresAt != out[b].ExpiresAt {
				return cmp.Compare(out[b].ExpiresAt, out[a].ExpiresAt)
			}
			return cmp.Compare(out[a].ResourceID, out[b].ResourceID)
		})
		for _, i := range indexes[min(len(indexes), relayGrantSessionsPerKey):] {
			drop[i] = true
		}
	}
	reduced := out[:0]
	for i, scope := range out {
		if !drop[i] {
			reduced = append(reduced, scope)
		}
	}
	// Distinct (kind, capability, direction) keys are few and fixed (scopeCapabilities), so this holds for every peer
	// that has fewer than MaxGrantScopes - keys*relayGrantSessionsPerKey standing scopes; a peer past it is cut last.
	return reduced[:min(len(reduced), MaxGrantScopes)]
}

// relayGrantSessionsPerKey is how many sessions of one (kind, capability, direction) a grant keeps over the bound.
const relayGrantSessionsPerKey = 4

// AccountNetworkID is the resource id of an account's personal mesh in a relay grant's memberships: one membership
// per direction pairs the endpoint with every endpoint of its account at the relay, instead of one per-pair
// machine_network scope per peer (PLAN 3b "Relay pair authorization by membership"). The server issues it and the CLI
// accepts it only while its configuration holds a mesh scope of its own account in that direction.
func AccountNetworkID(account string) string {
	sum := sha256.Sum256([]byte("paperboat-account-network\x00" + account))
	return "network_" + hex.EncodeToString(sum[:])
}

// RelayGrantMemberships is every membership a relay grant may carry for the scopes of an endpoint's relayable peers
// (one scope list per peer), each once, the account mesh first and then the latest expiry first. The server signs a
// bounded prefix of it (MaxMergedMemberships), for the peers its relay admits; the client builds the whole set from
// its own signed configuration to refuse any membership of a grant that is not in it, so both must use this one
// function. A per-pair machine_network scope becomes the account's one network (AccountNetworkID) in the direction
// the endpoint holds, and a peer with a mesh scope adds nothing else (the mesh pairs it); a peer without one (a team
// or another account's endpoint) adds its relay scopes (RelayGrantScopes), standing ones stamped with grantExpiry.
func RelayGrantMemberships(accountID string, peers [][]Scope, grantExpiry int64) []Scope {
	network := AccountNetworkID(accountID)
	seen := map[Scope]bool{}
	out := []Scope{}
	for _, peerScopes := range peers {
		mesh := slices.ContainsFunc(peerScopes, func(s Scope) bool { return s.ResourceKind == "machine_network" })
		for _, scope := range RelayGrantScopes(peerScopes, grantExpiry) {
			switch {
			case scope.ResourceKind == "machine_network":
				scope.ResourceID = network
			case mesh:
				continue
			}
			if !seen[scope] {
				seen[scope] = true
				out = append(out, scope)
			}
		}
	}
	rank := func(s Scope) int { // the mesh first
		if s.ResourceKind == "machine_network" {
			return 0
		}
		return 1
	}
	slices.SortFunc(out, func(a, b Scope) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(b.ExpiresAt, a.ExpiresAt), cmp.Compare(a.ResourceKind, b.ResourceKind), cmp.Compare(a.ResourceID, b.ResourceID),
			cmp.Compare(a.Capability, b.Capability), cmp.Compare(a.Direction, b.Direction), cmp.Compare(a.ResourceGeneration, b.ResourceGeneration))
	})
	return out
}
