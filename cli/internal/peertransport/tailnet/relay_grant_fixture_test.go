package tailnet

import (
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// grantScope is a configured scope as a relay grant membership carries it (the configuration's own values).
func grantScope(s NetworkScope) derpquic.Scope {
	return derpquic.Scope(s)
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

// peerGrantMemberships are the memberships the server signs for the configuration's peers in a grant of this expiry.
func peerGrantMemberships(cfg NetworkConfiguration, grantExpiry int64) []derpquic.Scope {
	var peers [][]derpquic.Scope
	for _, p := range cfg.Peers {
		peers = append(peers, p.Scopes)
	}
	return peernetwork.RelayGrantMemberships(cfg.Self.AccountID, peers, grantExpiry)
}
