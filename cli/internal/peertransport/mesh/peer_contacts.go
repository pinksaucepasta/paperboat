package mesh

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"time"

	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=direct-peer-contacts
	"tailscale.com/ipn/ipnstate"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=direct-peer-contacts
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=direct-peer-contacts
	"tailscale.com/types/key"
)

// PeerContact is what the signed peer configuration says about reaching a peer directly: its disco key and
// its own candidate UDP addresses (PLAN 3b "Direct paths without a relay").
type PeerContact struct {
	Disco     key.DiscoPublic
	Endpoints []netip.AddrPort
}

// maxPeerContactEndpoints matches the signed configuration's bound.
const maxPeerContactEndpoints = 8

// backgroundMeow bounds the relay handshake that still runs for a peer reached directly, so a peer that does
// not know our endpoints yet still learns that we may connect.
const backgroundMeow = 30 * time.Second

// SetPeerContacts replaces the direct contacts of the allowed peers. An allowed peer with endpoints gets its
// netmap node at once, so the engine disco-pings those addresses directly, and the peer's own handshakes are
// accepted with no relay in between; before, a node appeared only after a meow over a relay.
func (s *Server) SetPeerContacts(contacts map[key.NodePublic]PeerContact) error {
	for k, c := range contacts {
		if k.IsZero() || c.Disco.IsZero() || len(c.Endpoints) > maxPeerContactEndpoints {
			return errors.New("mesh: invalid peer contact")
		}
	}
	if s.lb == nil {
		s.PeerContacts = maps.Clone(contacts)
		return nil
	}
	b := s.lb
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	next := make(map[key.NodePublic]PeerContact, len(contacts))
	for k, c := range contacts {
		c.Endpoints = slices.Clone(c.Endpoints)
		next[k] = c
	}
	b.contacts = next // addClientLocked reads the new endpoints from here
	changed := false
	for k, c := range next {
		addr, allowed := b.allowedPeers[k]
		if !allowed || len(c.Endpoints) == 0 {
			continue
		}
		n := b.clients[k]
		if n == nil {
			b.addClientLocked(k, c.Disco, addr)
			changed = true
			continue
		}
		if !slices.Equal(n.Endpoints, c.Endpoints) {
			updated := n.Clone()
			updated.Endpoints = c.Endpoints
			b.setClientLocked(k, updated)
			changed = true
		}
	}
	if changed {
		b.updateServerMapLocked()
	}
	return nil
}

// addClientLocked adds the netmap node of an allowed peer with the endpoints its contact names; b.mu held.
func (b *locoBackend) addClientLocked(k key.NodePublic, disco key.DiscoPublic, addr netip.Addr) {
	if b.nextClientID < 2 {
		b.nextClientID = 2
	}
	id := b.nextClientID
	b.nextClientID++
	b.setClientLocked(k, &tailcfg.Node{
		ID:         id,
		StableID:   tailcfg.StableNodeID(fmt.Sprint(id)),
		Name:       fmt.Sprintf("peer%d.paperboat.", id),
		User:       100,
		Key:        k,
		DiscoKey:   disco,
		Addresses:  []netip.Prefix{pfxOf(addr)},
		AllowedIPs: []netip.Prefix{pfxOf(addr)},
		Endpoints:  slices.Clone(b.contacts[k].Endpoints),
		HomeDERP:   b.derpRegionIDLocked(),
		Cap:        tailcfg.CurrentCapabilityVersion,
	})
}

// directContact reports whether the signed configuration gave peer's endpoints.
func (b *locoBackend) directContact(peer key.NodePublic) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.contacts[peer].Endpoints) > 0
}

// LocalEndpoints are this engine's own candidate UDP addresses, the ones it advertises.
func (s *Server) LocalEndpoints() []netip.AddrPort {
	if s.lb == nil {
		return nil
	}
	s.lb.mu.Lock()
	defer s.lb.mu.Unlock()
	return slices.Clone(s.lb.eps)
}

// directPingWait bounds the disco ping that opens a direct path before the first WireGuard handshake.
const directPingWait = 3 * time.Second

// pingDirect disco-pings peer at its signed endpoints and waits for the answer, bounded by directPingWait.
// magicsock only starts pinging when a send finds no path, so without it the first handshakes find none
// and each side waits out a 5 s WireGuard retry (about 10 s for a first contact with no relay).
func (s *Server) pingDirect(ctx context.Context, peer key.NodePublic) {
	s.lb.mu.Lock()
	addr, ok := s.lb.allowedPeers[peer]
	s.lb.mu.Unlock()
	if !ok {
		return
	}
	answered := make(chan struct{})
	var once sync.Once
	s.lb.sys.Engine.Get().Ping(addr, tailcfg.PingDisco, 0, func(*ipnstate.PingResult) { once.Do(func() { close(answered) }) })
	wait := time.NewTimer(directPingWait)
	defer wait.Stop()
	select {
	case <-answered:
	case <-wait.C:
	case <-ctx.Done():
	}
}

// meowInBackground runs the relay handshake for a peer that is already reachable directly, bounded by
// backgroundMeow; a missing relay only ends it early.
func (s *Server) meowInBackground(peer key.NodePublic) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), backgroundMeow)
		defer cancel()
		_ = s.awaitMeow(ctx, peer)
	}()
}
