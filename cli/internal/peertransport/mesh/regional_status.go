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
