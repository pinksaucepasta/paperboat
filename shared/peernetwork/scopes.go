package peernetwork

import "slices"

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
	"recording":       {"recording"}, // a CLI session recording replay (shared/recording NativeConsumer)
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

// ValidGrantScope is the relay's rule for a grant scope, per peer or as a membership: a known kind and capability,
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
