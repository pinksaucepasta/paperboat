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

// awaitRelayOrDirect readies a peer whose endpoints the signed configuration gives: it returns as soon as the
// relay handshake is acknowledged (the relay carries traffic while no direct path is proven, as in Tailscale) or
// a disco ping to the endpoints is answered (a direct path is proven), whichever comes first. Returning after an
// unanswered ping left two machines behind one NAT, whose signed endpoints could not reach each other, without
// the relay: every attempt idled out (Lane H, support_25a0e3be). With no usable relay the ping alone decides.
func (s *Server) awaitRelayOrDirect(ctx context.Context, peer key.NodePublic) error {
	relay := make(chan error, 1)
	meowCtx, stopMeow := context.WithTimeout(context.WithoutCancel(ctx), backgroundMeow)
	go func() {
		defer stopMeow()
		relay <- s.awaitMeow(meowCtx, peer) // keeps running, bounded, after a direct answer: the peer learns we may connect
	}()
	direct := s.pingDirect(ctx, peer)
	for {
		select {
		case err := <-relay:
			if err == nil {
				return nil
			}
			relay = nil // the relay handshake ended without an answer; only a direct answer is left
		case <-direct:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
		if relay == nil && direct == nil {
			return errors.New("mesh: peer is reachable neither directly nor through a relay")
		}
	}
}

// pingDirect disco-pings peer at its signed endpoints and returns a channel closed when one answers.
func (s *Server) pingDirect(ctx context.Context, peer key.NodePublic) <-chan struct{} {
	answered := make(chan struct{})
	s.lb.mu.Lock()
	addr, ok := s.lb.allowedPeers[peer]
	s.lb.mu.Unlock()
	if !ok {
		return nil
	}
	var once sync.Once
	// A lost or early ping (the peer has not installed our contact yet) is retried with backoff until one answers or
	// ctx ends, as magicsock re-pings unconfirmed endpoints (tailscale wgengine/magicsock/endpoint.go sendDiscoPingsLocked).
	go func() {
		for wait := directPingFirstRetry; ; wait = min(2*wait, directPingMaxRetry) {
			s.lb.sys.Engine.Get().Ping(addr, tailcfg.PingDisco, 0, func(r *ipnstate.PingResult) {
				if r != nil && r.Err == "" {
					once.Do(func() { close(answered) })
				}
			})
			select {
			case <-answered:
				return
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
	return answered
}

const directPingFirstRetry, directPingMaxRetry = 250 * time.Millisecond, 2 * time.Second
