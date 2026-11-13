package peernetwork

import (
	"fmt"
	"testing"
)

func sessionScopes(direction string, sessions int, expiry func(int) int64) []Scope {
	var scopes []Scope
	for i := range sessions {
		for _, capability := range []string{"terminal", "file_transfer", "private_access"} {
			scopes = append(scopes, Scope{ResourceKind: "machine_access", ResourceID: fmt.Sprintf("session_%04d", i), ResourceGeneration: 1, Capability: capability, Direction: direction, Port: NetworkPort, ExpiresAt: expiry(i)})
		}
	}
	return scopes
}

// reciprocalExpiry is the relay's rule for a pair (relay/derpquic reciprocalScopes, allowedPairExpiry): the latest of
// min(both expiries) over scopes with the same resource, capability and generation and opposite directions.
func reciprocalExpiry(a, b []Scope) int64 {
	var latest int64
	for _, s := range a {
		for _, t := range b {
			if s.ResourceKind == t.ResourceKind && s.ResourceID == t.ResourceID && s.ResourceGeneration == t.ResourceGeneration && s.Capability == t.Capability && s.Direction != t.Direction {
				latest = max(latest, min(s.ExpiresAt, t.ExpiresAt))
			}
		}
	}
	return latest
}

// F21-164: more than 128 sessions between two endpoints. The cut must never end a pair a live session holds, so the
// pair's lifetime over the reduced grants equals the one over every session, even when the longest-lived session has the
// highest id (the document is sorted by id) and the two ends were built from the same set separately.
func TestRelayGrantScopesOverTheBoundKeepTheLongestLivedPair(t *testing.T) {
	const grantExpiry = 100_000
	expiry := func(i int) int64 { return int64(1_000 + (i*37)%900) } // spread, none the largest at the lowest id
	dial, accept := sessionScopes("dial", 300, expiry), sessionScopes("accept", 300, expiry)
	// The session that lives longest has the last id, so a cut in document order drops it.
	for _, set := range [][]Scope{dial, accept} {
		for i := range set {
			if set[i].ResourceID == "session_0299" {
				set[i].ExpiresAt = 5_000
			}
		}
	}
	want := reciprocalExpiry(dial, accept)
	if want != 5_000 {
		t.Fatalf("fixture: full pair lifetime %d", want)
	}
	a, b := RelayGrantScopes(dial, grantExpiry), RelayGrantScopes(accept, grantExpiry)
	if len(a) > MaxGrantScopes || len(b) > MaxGrantScopes {
		t.Fatalf("grants hold %d and %d scopes, the relay refuses over %d", len(a), len(b), MaxGrantScopes)
	}
	if got := reciprocalExpiry(a, b); got != want {
		t.Fatalf("pair lifetime over the grants is %d, over every session %d: a needed scope was cut", got, want)
	}
	for _, capability := range []string{"terminal", "file_transfer", "private_access"} {
		if got := reciprocalExpiry(filterCapability(a, capability), filterCapability(b, capability)); got != want {
			t.Fatalf("%s pair lifetime %d, want %d", capability, got, want)
		}
	}
}

func filterCapability(scopes []Scope, capability string) []Scope {
	var out []Scope
	for _, s := range scopes {
		if s.Capability == capability {
			out = append(out, s)
		}
	}
	return out
}

// An account's own peer carries its mesh pair, stamped with the grant expiry, which outlives every session: the
// reduced grant keeps it and the pair lasts the grant's life.
func TestRelayGrantScopesOverTheBoundKeepTheStandingScopes(t *testing.T) {
	const grantExpiry = 100_000
	scopes := sessionScopes("dial", 300, func(int) int64 { return 2_000 })
	scopes = append(scopes,
		Scope{ResourceKind: "machine_network", ResourceID: "mesh", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: NetworkPort},
		Scope{ResourceKind: "machine_network", ResourceID: "mesh", ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: NetworkPort})
	got := RelayGrantScopes(scopes, grantExpiry)
	if len(got) > MaxGrantScopes {
		t.Fatalf("grant holds %d scopes", len(got))
	}
	mesh := 0
	for _, s := range got {
		if s.ResourceKind == "machine_network" {
			mesh++
			if s.ExpiresAt != grantExpiry {
				t.Fatalf("mesh scope expiry %d, want the grant's", s.ExpiresAt)
			}
		}
	}
	if mesh != 2 {
		t.Fatalf("%d mesh scopes kept, want 2", mesh)
	}
}

// Up to the bound nothing is reduced: every session scope is carried as before.
func TestRelayGrantScopesUnderTheBoundAreUnchanged(t *testing.T) {
	scopes := sessionScopes("dial", 42, func(i int) int64 { return int64(1_000 + i) })
	if got := RelayGrantScopes(scopes, 9_000); len(got) != len(scopes) {
		t.Fatalf("%d of %d scopes carried", len(got), len(scopes))
	}
}

// Whatever mix of sessions a peer has, a grant stays within the bound: every (kind, capability, direction) is reduced
// to relayGrantSessionsPerKey, and those keys are few.
func TestRelayGrantScopesAreBoundedForEveryKey(t *testing.T) {
	var scopes []Scope
	Scopes(func(kind, capability string) bool {
		if kind == "machine_network" {
			return true
		}
		for _, direction := range []string{"dial", "accept"} {
			for i := range 50 {
				scopes = append(scopes, Scope{ResourceKind: kind, ResourceID: fmt.Sprintf("%s_%s_%d", kind, capability, i), ResourceGeneration: 1, Capability: capability, Direction: direction, Port: NetworkPort, ExpiresAt: int64(100 + i)})
			}
		}
		return true
	})
	got := RelayGrantScopes(scopes, 9_000)
	if len(got) == 0 || len(got) > MaxGrantScopes {
		t.Fatalf("%d scopes from %d", len(got), len(scopes))
	}
}
