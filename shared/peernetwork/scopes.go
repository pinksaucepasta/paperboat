package peernetwork

// scopeCapabilities lists, for each resource kind of a signed access scope, the capabilities it may carry.
// The client accepts exactly these (a configuration with any other scope is invalid as a whole) and the
// server's tests check every scope it can emit against them: a (machine_access, inspector) rule the client
// did not know made hosts reject every renewal for three minutes on 2026-10-27.
var scopeCapabilities = map[string][]string{
	"machine_access": {"terminal", "exec", "managed_ssh", "file_transfer", "private_access", "inspector"},
	"inspector":      {"inspector"},
	"config_compare": {"config_compare"},
	"recording":      {"recording"}, // a CLI session recording replay (shared/recording NativeConsumer)
	"codex_session":  {"codex"},
}

// ValidScope reports whether a signed access scope of kind may carry capability.
func ValidScope(kind, capability string) bool {
	for _, c := range scopeCapabilities[kind] {
		if c == capability {
			return true
		}
	}
	return false
}

// RelayGrantKeepsStanding reports whether a relay grant carries a scope held until revoked (expires_at 0); the grant
// stamps it with its own expiry, because relays refuse a scope without one. The server builds the grant and the CLI
// rebuilds the same set from its configuration, so both call this. A machine_network scope is kept. A machine_access
// scope is a session right that authorizes a relay pair only when no machine_network scope pairs the two endpoints
// (same account); it is kept for a peer without one, a team member's CLI and the shared host, whose pair the relay
// admits from reciprocal machine_access dial and accept scopes (derpquic reciprocalScopes). Keeping it for mesh peers
// as well would push 64 CLI peers x 4 capabilities past the relay's 128-scope grant bound (P0 2026-10-26).
func RelayGrantKeepsStanding(kind, capability, direction string, generation int64, port int, peerHasMesh bool) bool {
	switch kind {
	case "machine_network":
		return true
	case "machine_access":
		return !peerHasMesh && generation >= 1 && port == 443 && (direction == "dial" || direction == "accept") &&
			(capability == "terminal" || capability == "exec" || capability == "managed_ssh" || capability == "file_transfer")
	}
	return false
}
