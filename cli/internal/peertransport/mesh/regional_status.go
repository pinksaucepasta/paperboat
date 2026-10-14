// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-diagnostics
	"tailscale.com/ipn/ipnstate"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-diagnostics
	"tailscale.com/types/key"
)

// UsagePath describes a selected path without exposing peer addresses. NodeID
// stays empty when the engine cannot prove the identity of the carrying relay.
type UsagePath struct {
	Mode     string
	NodeID   string
	regional bool
}

// IsRegionalRelay distinguishes the regional carrier from a peer-relay path.
// Both are exposed as relay to users, but only the regional carrier produces
// the actual per-peer observations used by current usage accounting.
func (p UsagePath) IsRegionalRelay() bool { return p.regional }

// PeerUsagePath samples the currently observed path of an admitted peer. It is
// application accounting metadata, not an encrypted packet byte counter.
func (s *Server) PeerUsagePath(peer key.NodePublic) UsagePath {
	if s.lb == nil || peer.IsZero() {
		return UsagePath{Mode: "unknown"}
	}
	s.lb.mu.Lock()
	_, admitted := s.lb.allowedPeers[peer]
	s.lb.mu.Unlock()
	if !admitted {
		return UsagePath{Mode: "unknown"}
	}
	status := &ipnstate.StatusBuilder{WantPeers: true}
	s.lb.sys.MagicSock.Get().UpdateStatus(status)
	return usagePathFromStatus(status.Status().Peer[peer])
}

func usagePathFromStatus(p *ipnstate.PeerStatus) UsagePath {
	if p == nil || !p.Active {
		return UsagePath{Mode: "unknown"}
	}
	if p.CurAddr != "" {
		return UsagePath{Mode: "direct"}
	}
	if p.PeerRelay != "" {
		// The peer-relay allocation address does not prove registry identity.
		return UsagePath{Mode: "relay"}
	}
	if p.Relay != "" {
		// Status carries the peer's home region, which can differ from the
		// carrier actually used by reverse routing. Do not infer node identity.
		return UsagePath{Mode: "relay", regional: true}
	}
	return UsagePath{Mode: "unknown"}
}

// PeerPath reports the current data path for an admitted peer without exposing
// its network address. An idle peer has no selected path.
func (s *Server) PeerPath(peer key.NodePublic) string {
	if s.lb == nil || peer.IsZero() {
		return "unknown"
	}
	s.lb.mu.Lock()
	_, admitted := s.lb.allowedPeers[peer]
	s.lb.mu.Unlock()
	if !admitted {
		return "unknown"
	}
	status := &ipnstate.StatusBuilder{WantPeers: true}
	s.lb.sys.MagicSock.Get().UpdateStatus(status)
	p := status.Status().Peer[peer]
	if p == nil || !p.Active {
		return "unknown"
	}
	if p.CurAddr != "" {
		return "direct"
	}
	if p.PeerRelay != "" {
		return "peer_relay"
	}
	if p.Relay != "" {
		return "regional_relay"
	}
	return "unknown"
}

// RelayTransport reports the concrete transport carrying a server region.
func (s *Server) RelayTransport(id tailcfg.DERPRegionID) string {
	if s.lb == nil {
		return ""
	}
	return s.lb.sys.MagicSock.Get().RelayTransport(id)
}
