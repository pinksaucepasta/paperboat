// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"errors"
	"maps"
	"net/netip"
	"slices"

	"tailscale.com/types/key"
)

var authorityPrefix = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

func validAuthorityAddr(addr netip.Addr) bool {
	return addr.Zone() == "" && authorityPrefix.Contains(addr)
}

func validateAllowedPeers(local netip.Addr, peers map[key.NodePublic]netip.Addr) error {
	if !validAuthorityAddr(local) {
		return errors.New("mesh: invalid authority local address")
	}
	seen := map[netip.Addr]bool{local: true}
	for k, addr := range peers {
		if k.IsZero() || !validAuthorityAddr(addr) || seen[addr] {
			return errors.New("mesh: invalid or duplicate authority peer address/key")
		}
		seen[addr] = true
	}
	return nil
}

func (s *Server) validateAuthority() error {
	if s.Key.IsZero() {
		return errors.New("mesh: explicit node identity is required")
	}
	if s.AllowedPeers == nil {
		return errors.New("mesh: explicit peer policy is required")
	}
	if _, self := s.AllowedPeers[s.Key.Public()]; self {
		return errors.New("mesh: own key cannot be a peer")
	}
	return validateAllowedPeers(s.LocalAddr, s.AllowedPeers)
}

// ReplaceAllowedPeers atomically replaces authority admission. Nil and empty maps
// deny all peers. Removed or rebound peers are removed from the network map and
// active WireGuard machine before return. Callers must close their revoked flow
// leases as well. It may run concurrently with peer admission after Start; Start
// and Close must not run concurrently with this method.
func (s *Server) ReplaceAllowedPeers(peers map[key.NodePublic]netip.Addr) error {
	if !s.Key.IsZero() {
		if _, self := peers[s.Key.Public()]; self {
			return errors.New("mesh: own key cannot be a peer")
		}
	}
	if err := validateAllowedPeers(s.LocalAddr, peers); err != nil {
		return err
	}
	peers = maps.Clone(peers)
	if peers == nil {
		peers = make(map[key.NodePublic]netip.Addr)
	}
	if s.lb == nil {
		s.AllowedPeers = peers
		return s.validateAuthority()
	}
	b := s.lb
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	b.mu.Lock()
	if maps.Equal(b.allowedPeers, peers) {
		b.mu.Unlock()
		return nil
	}
	b.allowedPeers = peers
	// Replace the filter without reusing conntrack state from the old grants.
	b.setFilter(s.buildFilter())
	var removed []key.NodePublic
	for k, n := range b.clients {
		if addr, ok := peers[k]; !ok || !slices.Contains(n.Addresses, pfxOf(addr)) {
			b.deleteClientLocked(k)
			removed = append(removed, k)
		}
	}
	if len(removed) != 0 {
		b.updateServerMapLocked()
	}
	b.mu.Unlock()
	// SyncDevicePeer calls peerConfig, which takes b.mu. Never hold b.mu here.
	// A concurrent meow can only restore keys admitted by the new policy.
	for _, k := range removed {
		b.sys.Engine.Get().SyncDevicePeer(k)
		s.peerReadyMu.Lock()
		delete(s.peerReady, k)
		s.peerReadyMu.Unlock()
	}
	return nil
}
