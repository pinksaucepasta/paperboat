package tailnet

import "github.com/pinksaucepasta/paperboat/relay/derpquic"

// grantScope is a configured scope as a relay grant membership carries it (the configuration's own values).
func grantScope(s NetworkScope) derpquic.Scope {
	return derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt}
}

// grantMemberships are the memberships of a relay grant for scopes of the configuration: each one once, never a
// list of peers (the server's relayGrantMemberships).
func grantMemberships(scopes ...NetworkScope) []derpquic.Scope {
	type tuple struct {
		kind, id, capability, direction string
		generation                      uint64
		port                            uint16
	}
	seen := map[tuple]bool{}
	var out []derpquic.Scope
	for _, s := range scopes {
		k := tuple{s.ResourceKind, s.ResourceID, s.Capability, s.Direction, s.ResourceGeneration, s.Port}
		if !seen[k] {
			seen[k] = true
			out = append(out, grantScope(s))
		}
	}
	return out
}

// peerGrantMemberships are the memberships for every scope the configuration holds toward its peers.
func peerGrantMemberships(cfg NetworkConfiguration) []derpquic.Scope {
	var scopes []NetworkScope
	for _, p := range cfg.Peers {
		scopes = append(scopes, p.Scopes...)
	}
	return grantMemberships(scopes...)
}
