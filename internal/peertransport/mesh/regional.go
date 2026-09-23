// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"context"
	"errors"
	"fmt"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/wgengine/filter"
)

const maxRelayRegions = 32

func cloneRelayRegions(regions []*tailcfg.DERPRegion) ([]*tailcfg.DERPRegion, error) {
	if len(regions) > maxRelayRegions {
		return nil, fmt.Errorf("mesh: relay regions must contain at most %d entries", maxRelayRegions)
	}
	seen := make(map[tailcfg.DERPRegionID]bool, len(regions))
	out := make([]*tailcfg.DERPRegion, len(regions))
	for i, region := range regions {
		if region == nil || region.RegionID < 1 || region.RegionID > 65535 || seen[region.RegionID] {
			return nil, errors.New("mesh: relay region IDs must be unique and in 1..65535")
		}
		seen[region.RegionID] = true
		out[i] = region.Clone()
	}
	return out, nil
}

func derpMapOf(regions []*tailcfg.DERPRegion) *tailcfg.DERPMap {
	dm := &tailcfg.DERPMap{Regions: make(map[tailcfg.DERPRegionID]*tailcfg.DERPRegion, len(regions))}
	for _, region := range regions {
		dm.Regions[region.RegionID] = region
	}
	return dm
}

// SetRelayRegions replaces the live relay inventory. The first entry is the
// selected home region; unchanged region carriers remain open.
func (s *Server) SetRelayRegions(regions []*tailcfg.DERPRegion) error {
	cloned, err := cloneRelayRegions(regions)
	if err != nil {
		return err
	}
	if s.lb != nil {
		s.lb.setRelayRegions(cloned)
	}
	s.relayRegions = cloned
	s.Region = nil
	if len(cloned) != 0 {
		s.Region = cloned[0]
	}
	return nil
}

func (b *locoBackend) setRelayRegions(regions []*tailcfg.DERPRegion) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	dm := derpMapOf(regions)
	b.mu.Lock()
	b.dm = dm
	b.homeDERP = 0
	if len(regions) != 0 {
		b.homeDERP = regions[0].RegionID
	}
	b.updateServerMapLocked()
	b.mu.Unlock()
	b.sys.MagicSock.Get().SetDERPMap(dm)
}

// SetPeerRelayNodes atomically replaces discovery-only peer relay metadata.
func (s *Server) SetPeerRelayNodes(nodes []*tailcfg.Node) error {
	if s.lb == nil {
		s.PeerRelayNodes = nodes
		return nil
	}
	return s.lb.replacePeerRelayNodes(nodes, func() *filter.Filter { return s.buildFilterWithState(s.lb.sys.Engine.Get().GetFilter()) })
}

func (b *locoBackend) replacePeerRelayNodes(nodes []*tailcfg.Node, filterFor func() *filter.Filter) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	b.mu.Lock()
	if err := b.setPeerRelayNodes(nodes); err != nil {
		b.mu.Unlock()
		return err
	}
	if b.nm != nil {
		kept := b.nm.Peers[:0]
		for _, p := range b.nm.Peers {
			if p.ID() < 1<<60 {
				kept = append(kept, p)
			}
		}
		b.nm.Peers = kept
		b.appendRelayNodes(b.nm)
		b.sys.MagicSock.Get().SetNetworkMap(b.nm.SelfNode, b.nm.Peers)
	}
	b.mu.Unlock()
	b.setFilter(filterFor())
	return nil
}

// EnsureRelayPeer admits an authority-listed peer using its authenticated
// discovery key. It does not bypass the existing admission policy.
func (s *Server) EnsureRelayPeer(peer key.NodePublic, disco key.DiscoPublic) error {
	if s.lb == nil {
		return errors.New("mesh: server is not started")
	}
	if !s.lb.onMeow(peer, disco) {
		return errors.New("mesh: peer is not admitted")
	}
	return nil
}

// PrepareRelay proves that the selected region's owned carrier can connect and ping.
func (s *Server) PrepareRelay(ctx context.Context, id tailcfg.DERPRegionID) error {
	if s.lb == nil {
		return errors.New("mesh: server is not started")
	}
	return s.lb.sys.MagicSock.Get().PrepareDERP(ctx, id)
}

func (s *Server) SetPeerRelayRegion(peer key.NodePublic, id tailcfg.DERPRegionID) error {
	if s.lb == nil {
		return errors.New("mesh: server is not started")
	}
	return s.lb.setPeerRelayRegion(peer, id)
}

func (b *locoBackend) setPeerRelayRegion(peer key.NodePublic, id tailcfg.DERPRegionID) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dm.Regions[id] == nil {
		return errors.New("mesh: unknown relay region")
	}
	n := b.clients[peer]
	if n == nil {
		return errors.New("mesh: peer is not admitted")
	}
	n = n.Clone()
	n.HomeDERP = id
	b.clients[peer] = n
	b.updateServerMapLocked()
	return nil
}

func (b *locoBackend) relayPeerAdmitted(peer key.NodePublic) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.relayControlPeers[peer] {
		return true
	}
	for _, relay := range b.peerRelayNodes {
		if relay != nil && relay.Key == peer {
			return true
		}
	}
	return b.allowedPeers[peer].IsValid()
}

func (b *locoBackend) setRelayControlPeers(peers []key.NodePublic) error {
	if len(peers) > 16 {
		return errors.New("mesh: too many relay control peers")
	}
	next := make(map[key.NodePublic]bool, len(peers))
	for _, peer := range peers {
		if peer.IsZero() || peer == b.pub || next[peer] {
			return errors.New("mesh: invalid relay control peer")
		}
		next[peer] = true
	}
	b.relayControlPeers = next
	return nil
}

func (s *Server) SetRelayControlPeers(peers []key.NodePublic) error {
	if s.lb == nil {
		s.RelayControlPeers = append([]key.NodePublic(nil), peers...)
		return nil
	}
	s.lb.mu.Lock()
	defer s.lb.mu.Unlock()
	return s.lb.setRelayControlPeers(peers)
}

func (s *Server) SendRelayControl(peer key.NodePublic, id tailcfg.DERPRegionID, payload []byte) error {
	if s.lb == nil {
		return errors.New("mesh: server is not started")
	}
	if !s.lb.relayPeerAdmitted(peer) {
		return errors.New("mesh: peer is not admitted")
	}
	return s.lb.sys.MagicSock.Get().SendDERPPacketToRegion(peer, id, payload)
}
