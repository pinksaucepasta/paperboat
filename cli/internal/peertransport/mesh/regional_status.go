// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"sync"
	"time"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-diagnostics
	"tailscale.com/ipn/ipnstate"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=peer-path-diagnostics
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=engine-assembly
	"tailscale.com/util/mak"
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
	return s.usagePaths.get(peer, time.Now(), func() UsagePath {
		status := &ipnstate.StatusBuilder{WantPeers: true}
		s.lb.sys.MagicSock.Get().UpdateStatus(status)
		return usagePathFromStatus(status.Status().Peer[peer])
	})
}

// usagePathTTL bounds how stale an accounting path may be. Metered connections ask for the path before
// and after every read and write, and a fresh answer builds magicsock's status of every peer to read
// one; a path switch is attributed at most this late.
const usagePathTTL = time.Second

// maxUsagePaths bounds the cache; stale entries are dropped once it holds more peers than this.
const maxUsagePaths = 1024

type usagePathCache struct {
	mu    sync.Mutex
	paths map[key.NodePublic]cachedUsagePath
}

type cachedUsagePath struct {
	path UsagePath
	at   time.Time
}

func (c *usagePathCache) get(peer key.NodePublic, now time.Time, compute func() UsagePath) UsagePath {
	c.mu.Lock()
	if cached, ok := c.paths[peer]; ok && now.Sub(cached.at) < usagePathTTL {
		c.mu.Unlock()
		return cached.path
	}
	c.mu.Unlock()
	path := compute()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.paths) >= maxUsagePaths {
		for k, cached := range c.paths {
			if now.Sub(cached.at) >= usagePathTTL {
				delete(c.paths, k)
			}
		}
	}
	mak.Set(&c.paths, peer, cachedUsagePath{path: path, at: now})
	return path
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
	if path, ok := s.PeerPaths([]key.NodePublic{peer})[peer]; ok {
		return path
	}
	return "unknown"
}

// PeerPaths reports the current data path (direct, peer_relay or regional_relay) of each admitted, active
// peer from one magicsock status, so a caller with many peers reads the status once. Idle peers are left out.
func (s *Server) PeerPaths(peers []key.NodePublic) map[key.NodePublic]string {
	paths := make(map[key.NodePublic]string, len(peers))
	if s.lb == nil || len(peers) == 0 {
		return paths
	}
	s.lb.mu.Lock()
	admitted := make([]key.NodePublic, 0, len(peers))
	for _, peer := range peers {
		if _, ok := s.lb.allowedPeers[peer]; ok && !peer.IsZero() {
			admitted = append(admitted, peer)
		}
	}
	s.lb.mu.Unlock()
	if len(admitted) == 0 {
		return paths
	}
	status := &ipnstate.StatusBuilder{WantPeers: true}
	s.lb.sys.MagicSock.Get().UpdateStatus(status)
	all := status.Status().Peer
	for _, peer := range admitted {
		p := all[peer]
		switch {
		case p == nil || !p.Active:
		case p.CurAddr != "":
			paths[peer] = "direct"
		case p.PeerRelay != "":
			paths[peer] = "peer_relay"
		case p.Relay != "":
			paths[peer] = "regional_relay"
		}
	}
	return paths
}

// RelayTransport reports the concrete transport carrying a server region.
func (s *Server) RelayTransport(id tailcfg.DERPRegionID) string {
	if s.lb == nil {
		return ""
	}
	return s.lb.sys.MagicSock.Get().RelayTransport(id)
}
