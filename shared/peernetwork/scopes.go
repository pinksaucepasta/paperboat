package peernetwork

// scopeCapabilities lists, for each resource kind of a signed access scope, the capabilities it may carry.
// The client accepts exactly these (a configuration with any other scope is invalid as a whole) and the
// server's tests check every scope it can emit against them: a (machine_access, inspector) rule the client
// did not know made hosts reject every renewal for three minutes on 2026-10-27.
var scopeCapabilities = map[string][]string{
	"machine_access": {"terminal", "exec", "managed_ssh", "file_transfer", "private_access", "inspector"},
	"inspector":      {"inspector"},
	"config_compare": {"config_compare"},
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
