// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"sync"
	"time"

	"go4.org/netipx"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/disco"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/health"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/ipn"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/ipn/store/mem"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/net/dns"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/net/netmon"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/net/netns"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/net/tsaddr"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/net/tsdial"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tailcfg/peercap"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/tsd"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/ipproto"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/logger"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/netmap"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/nettype"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/types/views"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/util/eventbus"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/util/mak"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/wgengine"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/wgengine/filter"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/wgengine/filter/filtertype"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/wgengine/magicsock"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/wgengine/netstack"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/wgengine/router"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=upstream-engine-assembly
	"tailscale.com/wgengine/wgcfg"
)

// locoBackend owns the engine subsystems and admitted network map. It retains
// Tailcat's assembly pattern without a Tailscale control client.
type locoBackend struct {
	testOnlyPacketListener nettype.PacketListener
	peerRelayNodes         []*tailcfg.Node
	relayControlPeers      map[key.NodePublic]bool
	derpCarrierFactory     magicsock.DERPCarrierFactory
	sys                    tsd.System
	priv                   key.NodePrivate
	pub                    key.NodePublic
	addr                   netip.Addr
	addrPrefix             netip.Prefix
	ns                     *netstack.Impl
	dm                     *tailcfg.DERPMap
	homeDERP               tailcfg.DERPRegionID
	logf                   logger.Logf
	allowedPeers           map[key.NodePublic]netip.Addr
	policyMu               sync.Mutex // serializes policy replacement through device synchronization
	nextClientID           tailcfg.NodeID

	// discoPublic returns the node's disco public key, memoized to
	// avoid redoing the curve25519 derivation for every client that
	// joins.
	discoPublic func() key.DiscoPublic

	// onDERPRecv is called for non-disco DERP packets before the
	// peer map lookup. Set before createEngine.
	onDERPRecv func(regionID tailcfg.DERPRegionID, src key.NodePublic, pkt []byte) bool

	mu        sync.Mutex
	clients   map[key.NodePublic]*tailcfg.Node // for the server
	nm        *netmap.NetworkMap
	eps       []netip.AddrPort // our current local UDP endpoints, sorted
	closeOnce sync.Once
}

func (b *locoBackend) derpRegionID() tailcfg.DERPRegionID {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.derpRegionIDLocked()
}

// derpRegionIDLocked returns the selected local home region. b.mu must be held.
func (b *locoBackend) derpRegionIDLocked() tailcfg.DERPRegionID {
	if b.dm == nil {
		panic("no derp map")
	}
	if b.homeDERP != 0 && b.dm.Regions[b.homeDERP] != nil {
		return b.homeDERP
	}
	for _, r := range b.dm.Regions {
		return r.RegionID
	}
	return 0
}

func (b *locoBackend) peerDERPRegion(peer key.NodePublic) tailcfg.DERPRegionID {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nm != nil {
		for _, n := range b.nm.Peers {
			if n.Key() == peer && n.HomeDERP() != 0 {
				return n.HomeDERP()
			}
		}
	}
	return b.derpRegionIDLocked()
}

func (b *locoBackend) Close() error {
	b.closeOnce.Do(func() {
		if b.ns != nil {
			b.ns.Close()
		}
		if e, ok := b.sys.Engine.GetOK(); ok {
			e.Close()
		}
		if m, ok := b.sys.NetMon.GetOK(); ok {
			m.Close()
		}
		if d, ok := b.sys.Dialer.GetOK(); ok {
			d.Close()
		}
		if bus, ok := b.sys.Bus.GetOK(); ok {
			bus.Close()
		}
	})
	return nil
}

// Server owns one userspace WireGuard engine for an explicitly authorized endpoint.
// Set configuration before Start. Live policy changes use the replacement methods.
// An empty peer map denies all peers; a zero identity or missing allocation is invalid.
type Server struct {
	TestOnlyPacketListener nettype.PacketListener
	PeerRelayNodes         []*tailcfg.Node
	RelayControlPeers      []key.NodePublic
	OnRelayControl         func(tailcfg.DERPRegionID, key.NodePublic, []byte) bool
	DERPCarrierFactory     magicsock.DERPCarrierFactory
	Key                    key.NodePrivate
	LocalAddr              netip.Addr
	AllowedPeers           map[key.NodePublic]netip.Addr
	Logf                   logger.Logf
	// Region is an explicitly supplied bootstrap relay. Nil permits direct-only startup.
	Region *tailcfg.DERPRegion
	// OnUDP dispatches local datagram flows. Nil rejects all new inbound flows,
	// including late replies to retired outbound sockets; there is no host forwarding.
	OnUDP func(uint16) func(ConnPacketConn)
	// ServedUDPPorts restricts inbound ports. Empty denies all new inbound flows.
	ServedUDPPorts []filter.PortRange
	UDPIdleTimeout time.Duration
	lb             *locoBackend
	relayRegions   []*tailcfg.DERPRegion
	peerReadyMu    sync.Mutex
	peerReady      map[key.NodePublic]chan struct{}
}

// Start installs the explicit identity, allocation and peer policy before networking.
// No external relay inventory or identity is discovered implicitly.
func (s *Server) Start() error {
	if err := s.validateAuthority(); err != nil {
		return err
	}
	if s.lb != nil {
		return errors.New("mesh: Server.Start called twice")
	}
	if s.UDPIdleTimeout < 0 {
		return errors.New("mesh: Server.UDPIdleTimeout must not be negative")
	}
	logf := s.Logf
	if logf == nil {
		logf = logger.Discard
	}
	priv := s.Key
	reg := s.Region
	if reg != nil && reg.RegionID == 0 {
		return fmt.Errorf("missing RegionID in %v", logger.AsJSON(reg))
	}

	lb := newLocoBackend(priv)
	lb.derpCarrierFactory = s.DERPCarrierFactory
	lb.testOnlyPacketListener = s.TestOnlyPacketListener
	lb.addr, lb.addrPrefix = s.LocalAddr, pfxOf(s.LocalAddr)
	lb.allowedPeers = maps.Clone(s.AllowedPeers)
	if err := lb.setPeerRelayNodes(s.PeerRelayNodes); err != nil {
		return err
	}
	if err := lb.setRelayControlPeers(s.RelayControlPeers); err != nil {
		return err
	}
	lb.logf = logf
	lb.dm = &tailcfg.DERPMap{}
	regions := s.relayRegions
	if len(regions) == 0 && reg != nil {
		regions = []*tailcfg.DERPRegion{reg}
	}
	for _, region := range regions {
		mak.Set(&lb.dm.Regions, region.RegionID, region)
	}
	if reg != nil {
		lb.homeDERP = reg.RegionID
	}

	sys := &lb.sys
	bus := eventbus.New()
	sys.Set(bus)
	sys.Set(health.NewTracker(bus))

	netMon, err := netmon.New(bus, func(format string, args ...any) {
		logf(format, args...)
	})
	if err != nil {
		lb.Close() // closes the subsystems started so far
		return fmt.Errorf("netmon.New: %w", err)
	}
	sys.Set(netMon)

	dialer := &tsdial.Dialer{Logf: logf} // mutated below (before used)
	sys.Set(dialer)

	var store ipn.StateStore = new(mem.Store)
	sys.Set(store)

	lb.onDERPRecv = func(regionID tailcfg.DERPRegionID, src key.NodePublic, pkt []byte) bool {
		if s.OnRelayControl != nil && s.OnRelayControl(regionID, src, pkt) {
			return true
		}
		if !IsMeowPacket(pkt) {
			return false
		}
		if IsMeowedPacket(pkt) {
			s.peerReadyMu.Lock()
			ready := s.peerReady[src]
			if ready != nil {
				select {
				case <-ready:
				default:
					close(ready)
				}
			}
			s.peerReadyMu.Unlock()
			return true
		}
		if _, discoPub, ok := ParseMeowPing(pkt); ok {
			mc := lb.sys.MagicSock.Get()
			go func() {
				// Only reply once the client is fully added as a peer:
				// "meowed" is the ack that tells the client it can
				// start dialing. Disallowed clients get no reply.
				if lb.onMeow(src, discoPub) {
					mc.SendDERPPacketTo(src, regionID, EncodeMeowed())
				}
			}()
			return true
		}
		return false
	}

	if err := createEngine(logf, lb); err != nil {
		lb.Close()
		return fmt.Errorf("createEngine: %w", err)
	}
	ns, err := newNetstack(logf, sys)
	if err != nil {
		lb.Close()
		return fmt.Errorf("newNetstack: %w", err)
	}
	ns.ProcessLocalIPs = true
	ns.ProcessSubnets = false
	ns.GetTCPHandlerForFlow = func(src, dst netip.AddrPort) (func(net.Conn), bool) {
		return nil, true
	}
	ns.GetUDPHandlerForFlow = func(src, dst netip.AddrPort) (handler func(nettype.ConnPacketConn), intercept bool) {
		var h func(ConnPacketConn)
		if dst.Addr() == lb.addr {
			if s.OnUDP != nil {
				h = s.OnUDP(dst.Port())
			}
		}
		if h == nil {
			return nil, true
		}
		return func(c nettype.ConnPacketConn) { h(newIdlePacketConn(c, s.udpIdleTimeout())) }, true
	}
	lb.ns = ns
	sys.Set(ns)

	dialer.UseNetstackForIP = func(ip netip.Addr) bool {
		_, ok := lb.peerByIP(ip)
		return ok
	}
	dialer.NetstackDialTCP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		return ns.DialContextTCP(ctx, dst)
	}
	dialer.NetstackDialUDP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		panic("unreachable from tailcat") // but required by Dialer currently
	}

	sys.Tun.Get().Start()

	s.lb = lb
	lb.setFilter(s.buildFilter())
	if err := lb.Start(); err != nil {
		s.lb = nil
		lb.Close()
		return err
	}
	return nil
}

func (s *Server) udpIdleTimeout() time.Duration {
	if s.UDPIdleTimeout != 0 {
		return s.UDPIdleTimeout
	}
	return DefaultUDPIdleTimeout
}

// buildFilter admits only authorized sources to explicitly served local UDP ports.
func (s *Server) buildFilter() *filter.Filter { return s.buildFilterWithState(nil) }

func (s *Server) buildFilterWithState(state *filter.Filter) *filter.Filter {
	lb := s.lb
	sources := make([]netip.Prefix, 0, len(lb.allowedPeers))
	for _, addr := range lb.allowedPeers {
		sources = append(sources, pfxOf(addr))
	}
	var matches []filter.Match
	if s.OnUDP != nil && len(s.ServedUDPPorts) != 0 {
		dsts := make([]filter.NetPortRange, 0, len(s.ServedUDPPorts))
		for _, ports := range s.ServedUDPPorts {
			dsts = append(dsts, filter.NetPortRange{Net: lb.addrPrefix, Ports: ports})
		}
		matches = append(matches, filter.Match{IPProto: views.SliceOf([]ipproto.Proto{ipproto.UDP}), Srcs: sources, Dsts: dsts})
	}
	var localNets netipx.IPSetBuilder
	localNets.AddPrefix(lb.addrPrefix)
	local, _ := localNets.IPSet()
	return filter.New(append(matches, lb.relayFilterMatches()...), nil, local, tailcatULASet(), state, lb.logf)
}

// Addr returns the server's authority-allocated IPv6 address.
// It must only be called after [Server.Start].
func (s *Server) Addr() netip.Addr { return s.lb.addr }

// DialAuthorizedUDP opens one outbound connected UDP flow to an authority-listed
// peer through this server's single shared WireGuard engine. The meow exchange is
// symmetric: it installs the peer locally and proves the remote engine has admitted
// this node before application traffic is sent. No inbound application port is
// enabled by this operation.
func (s *Server) DialAuthorizedUDP(ctx context.Context, peer key.NodePublic, disco key.DiscoPublic, ap netip.AddrPort) (ConnPacketConn, error) {
	if ctx == nil || s.lb == nil || peer.IsZero() || disco.IsZero() || !ap.IsValid() {
		return nil, errors.New("mesh: invalid authorized UDP peer")
	}
	s.lb.mu.Lock()
	address := s.lb.allowedPeers[peer]
	s.lb.mu.Unlock()
	if ap.Addr() != address {
		return nil, errors.New("mesh: invalid authorized UDP peer")
	}
	if err := s.prepareAuthorizedPeer(ctx, peer, disco); err != nil {
		return nil, err
	}
	return s.lb.ns.DialContextUDPWithBind(ctx, s.lb.addr, ap)
}

func (s *Server) prepareAuthorizedPeer(ctx context.Context, peer key.NodePublic, disco key.DiscoPublic) error {
	if !s.lb.onMeow(peer, disco) {
		return errors.New("mesh: peer is not admitted")
	}
	s.peerReadyMu.Lock()
	if s.peerReady == nil {
		s.peerReady = make(map[key.NodePublic]chan struct{})
	}
	ready := s.peerReady[peer]
	if ready == nil {
		ready = make(chan struct{})
		s.peerReady[peer] = ready
	}
	s.peerReadyMu.Unlock()
	mc := s.lb.sys.MagicSock.Get()
	packet := EncodeMeowPing(s.lb.pub, mc.DiscoPublicKey())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastSendErr error
	for {
		// Regional recovery may promote a different rendezvous while bootstrap is
		// waiting for admission. Retry on the current route, not the initial one.
		region := s.lb.peerDERPRegion(peer)
		lastSendErr = mc.SendDERPPacketToRegion(peer, region, packet)
		select {
		case <-ready:
			return nil
		case <-ctx.Done():
			s.peerReadyMu.Lock()
			if s.peerReady[peer] == ready {
				delete(s.peerReady, peer)
			}
			s.peerReadyMu.Unlock()
			return errors.Join(ctx.Err(), lastSendErr)
		case <-ticker.C:
		}
	}
}

// InvalidateAuthorizedPeer discards one peer's learned endpoints and requires
// the next outbound flow to repeat admission. It leaves unrelated peers and
// the shared engine running.
func (s *Server) InvalidateAuthorizedPeer(peer key.NodePublic) {
	s.peerReadyMu.Lock()
	delete(s.peerReady, peer)
	s.peerReadyMu.Unlock()
	if s.lb == nil {
		return
	}
	b := s.lb
	b.policyMu.Lock()
	b.mu.Lock()
	_, admitted := b.allowedPeers[peer]
	_, present := b.clients[peer]
	if admitted && present {
		delete(b.clients, peer)
		b.updateServerMapLocked()
	}
	b.mu.Unlock()
	if admitted && present {
		b.sys.Engine.Get().SyncDevicePeer(peer)
	}
	b.policyMu.Unlock()
}

// Close shuts down the server, closing the WireGuard engine and DERP connections.
func (s *Server) Close() error {
	if s.lb == nil {
		return nil // never started
	}
	return s.lb.Close()
}

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
// active WireGuard device before return. Callers must close their revoked flow
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
			delete(b.clients, k)
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

// TailcatAddr returns the tailcat address that clients use to connect to this
// server. It embeds the full DERP region, so clients don't need to
// fetch the DERP map from the network. It must only be called after
// [Server.Start].
func (s *Server) TailcatAddr() Addr {
	return s.lb.tailcatAddr()
}

func newLocoBackend(priv key.NodePrivate) *locoBackend {
	lb := &locoBackend{logf: logger.Discard, priv: priv, pub: priv.Public()}
	lb.discoPublic = sync.OnceValue(func() key.DiscoPublic { return discoPrivateForNode(lb.priv).Public() })
	return lb
}

func (b *locoBackend) selfAllowedIPs() []netip.Prefix { return []netip.Prefix{b.addrPrefix} }

// peerConfig returns the WireGuard config for the peer with public key k.
// It is the engine's per-peer
// WireGuard config source (see [wgengine.Engine.SetPeerConfigFunc]).
func (b *locoBackend) peerConfig(k key.NodePublic) (_ wgcfg.PeerConfig, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.clients[k]
	if !ok {
		return wgcfg.PeerConfig{}, false
	}
	return wgcfg.PeerConfig{AllowedIPs: n.AllowedIPs}, true
}

// peerByIP returns the public key of the peer that outbound packets
// addressed to dst should be sent to (see
// [wgengine.Engine.SetPeerByIPPacketFunc]).
func (b *locoBackend) peerByIP(dst netip.Addr) (_ key.NodePublic, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, n := range b.clients {
		if slices.Contains(n.Addresses, pfxOf(dst)) {
			return k, true
		}
	}
	return key.NodePublic{}, false
}

// peerForIP is the [wgengine.Engine.SetPeerForIPFunc] callback,
// mapping a tailcat IPv6 address to its node in the network map. The
// engine uses it to find the peer for [wgengine.Engine.Ping].
func (b *locoBackend) peerForIP(ip netip.Addr) (_ wgengine.PeerForIP, ok bool) {
	var zero wgengine.PeerForIP
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nm == nil {
		return zero, false
	}
	if nodeHasAddr(b.nm.SelfNode, ip) {
		return wgengine.PeerForIP{Node: b.nm.SelfNode, IsSelf: true}, true
	}
	for _, p := range b.nm.Peers {
		if nodeHasAddr(p, ip) {
			return wgengine.PeerForIP{Node: p}, true
		}
	}
	return zero, false
}

// onEngineStatus is the wgengine status callback. It watches for
// changes to our magicsock UDP endpoints (learned via STUN and from
// local interfaces) and advertises them to all current peers. Tailcat
// has no control plane distributing endpoints, and magicsock never
// attempts a direct path to a peer with no known endpoints, so these
// advertisements are what make direct connections possible at all.
func (b *locoBackend) onEngineStatus(st *wgengine.Status, err error) {
	if err != nil || st == nil {
		return
	}
	var eps []netip.AddrPort
	for _, ep := range st.LocalAddrs {
		eps = append(eps, ep.Addr)
	}
	slices.SortFunc(eps, func(a, b netip.AddrPort) int { return a.Compare(b) })
	eps = slices.Compact(eps)
	b.mu.Lock()
	changed := !slices.Equal(eps, b.eps)
	if changed {
		b.eps = eps
	}
	b.mu.Unlock()
	if changed && len(eps) > 0 {
		go b.advertiseEndpoints()
	}
}

// advertiseEndpoints sends our current UDP endpoints to every known
// peer in a disco CallMeMaybe message over DERP, the same message the
// control plane flow uses in regular Tailscale. The peer's magicsock
// reacts by disco-pinging those endpoints, which both teaches it a
// direct path to us and teaches us its address from the pings we
// receive back. It's called whenever our endpoints change and when a
// peer first completes the meow handshake, and is idempotent.
func (b *locoBackend) advertiseEndpoints() {
	if runtime.GOOS == "js" {
		// In browsers this does nothing: js/wasm has no UDP, so
		// peers disco-pinging whatever endpoints magicsock reports
		// could never reach us. Direct browser connections would
		// need WebRTC; see
		// https://github.com/tailscale/tailcat/issues/4.
		return
	}
	b.mu.Lock()
	eps := slices.Clone(b.eps)
	var peers []tailcfg.NodeView
	if b.nm != nil {
		peers = b.nm.Peers
	}
	b.mu.Unlock()
	if len(eps) == 0 || len(peers) == 0 {
		return
	}
	payload := (&disco.CallMeMaybe{MyNumber: eps}).AppendMarshal(nil)
	discoPriv := discoPrivateForNode(b.priv)
	mc := b.sys.MagicSock.Get()
	regionID := b.derpRegionID()
	for _, p := range peers {
		// Frame and seal the message the same way magicsock's
		// sendDiscoMessage does, so the peer's stock magicsock
		// processes it natively.
		pkt := make([]byte, 0, 512)
		pkt = append(pkt, disco.Magic...)
		pkt = b.discoPublic().AppendTo(pkt)
		pkt = append(pkt, discoPriv.Shared(p.DiscoKey()).Seal(payload)...)
		if _, err := mc.SendDERPPacketTo(p.Key(), regionID, pkt); err != nil {
			b.logf("advertiseEndpoints to %v: %v", p.Key().ShortString(), err)
		}
	}
}

// nodeHasAddr reports whether ip is one of n's tailcat addresses.
func nodeHasAddr(n tailcfg.NodeView, ip netip.Addr) bool {
	if !n.Valid() {
		return false
	}
	for _, pfx := range n.Addresses().All() {
		if pfx.Addr() == ip {
			return true
		}
	}
	return false
}

func (lb *locoBackend) Start() error {
	if err := lb.ns.Start(nil /* no LocalBackend */); err != nil {
		return fmt.Errorf("failed to start netstack: %w", err)
	}

	e := lb.sys.Engine.Get()
	mc := lb.sys.MagicSock.Get()
	lb.logf("disco pub key: %v", mc.DiscoPublicKey())

	mc.SetPrivateKey(lb.priv)
	mc.SetDERPMap(lb.dm)

	derpRegion := lb.derpRegionID()

	nm := &netmap.NetworkMap{
		NodeKey: lb.pub,
	}
	nm.SelfNode = (&tailcfg.Node{
		ID:         1,
		StableID:   "1",
		Name:       "endpoint.paperboat.",
		User:       100,
		Key:        lb.pub,
		DiscoKey:   mc.DiscoPublicKey(),
		Addresses:  []netip.Prefix{lb.addrPrefix},
		AllowedIPs: lb.selfAllowedIPs(),
		HomeDERP:   derpRegion,
		Cap:        tailcfg.CurrentCapabilityVersion,
	}).View()
	lb.appendRelayNodes(nm)
	lb.mu.Lock()
	lb.nm = nm
	lb.mu.Unlock()

	mc.SetNetworkMap(nm.SelfNode, nm.Peers)
	e.SetSelfNode(nm.SelfNode)
	lb.sys.Netstack.Get().UpdateNetstackIPs(nm)
	mc.SetNetworkUp(true)
	lb.logf("NetworkMap: %v", logger.AsJSON(nm))

	// Install the live per-peer config sources. WireGuard peers are
	// created lazily from these as traffic arrives; there is no
	// peer list in wgcfg.Config anymore.
	e.SetPeerConfigFunc(lb.peerConfig)
	e.SetPeerByIPPacketFunc(lb.peerByIP)
	e.SetPeerForIPFunc(lb.peerForIP)
	e.SetStatusCallback(lb.onEngineStatus)

	wgConf := &wgcfg.Config{
		PrivateKey: lb.priv,
		Addresses:  []netip.Prefix{lb.addrPrefix},
	}
	routerConf := &router.Config{
		LocalAddrs: []netip.Prefix{lb.addrPrefix},
	}
	dnsConf := &dns.Config{}
	if err := e.Reconfig(wgConf, routerConf, dnsConf); err != nil {
		return fmt.Errorf("e.Reconfig: %w", err)
	}
	lb.sys.NetMon.Get().Start()

	return nil
}

// onMeow handles a MeowPing from the client with node key src and
// disco key discoPub, adding it as a WireGuard peer. It reports
// whether the client is allowed and configured, meaning a "meowed"
// acknowledgment may be sent.
func (b *locoBackend) onMeow(src key.NodePublic, discoPub key.DiscoPublic) bool {
	if src.IsZero() || discoPub.IsZero() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logf("got meow from %v", src.String())
	if !b.allowedPeers[src].IsValid() {
		b.logf("ignoring meow from %v: not in allowedClients", src.String())
		return false
	}

	if _, ok := b.clients[src]; ok {
		return true
	}
	if b.nextClientID < 2 {
		b.nextClientID = 2
	}
	id := b.nextClientID
	b.nextClientID++
	addr := b.allowedPeers[src]
	derpRegion := b.derpRegionIDLocked()
	mak.Set(&b.clients, src, &tailcfg.Node{
		ID:         tailcfg.NodeID(id),
		StableID:   tailcfg.StableNodeID(fmt.Sprint(id)),
		Name:       fmt.Sprintf("peer%d.paperboat.", id),
		User:       100,
		Key:        src,
		DiscoKey:   discoPub,
		Addresses:  []netip.Prefix{pfxOf(addr)},
		AllowedIPs: []netip.Prefix{pfxOf(addr)},
		HomeDERP:   derpRegion,
		Cap:        tailcfg.CurrentCapabilityVersion,
	})

	b.updateServerMapLocked()

	// No engine reconfig needed: the WireGuard device learns about the
	// new peer lazily via the config source installed with
	// SetPeerConfigFunc when the client's handshake arrives.

	// Tell the new client our UDP endpoints so both sides can attempt
	// a direct path. Async because advertiseEndpoints takes b.mu.
	go b.advertiseEndpoints()
	return true
}

// setPeerRelayNodes copies only validated service nodes into discovery state.
// Relay identity does not grant WireGuard/application access: peerConfig and
// peerByIP continue to use the authorized server/client inventory exclusively.
func (b *locoBackend) setPeerRelayNodes(nodes []*tailcfg.Node) error {
	if len(nodes) > 32 {
		return errors.New("mesh: too many peer relay services")
	}
	keys := map[key.NodePublic]bool{}
	addresses := map[netip.Addr]bool{}
	copied := make([]*tailcfg.Node, 0, len(nodes))
	for i, n := range nodes {
		if n == nil || n.Key.IsZero() || n.DiscoKey.IsZero() || n.Key == b.pub || keys[n.Key] || n.HomeDERP == 0 || len(n.Addresses) != 1 || !n.Addresses[0].IsSingleIP() || !validAuthorityAddr(n.Addresses[0].Addr()) || n.Addresses[0].Addr() == b.addr || addresses[n.Addresses[0].Addr()] {
			return errors.New("mesh: invalid peer relay service")
		}
		keys[n.Key] = true
		addresses[n.Addresses[0].Addr()] = true
		relay := n.Clone()
		// Keep internal IDs disjoint from server=1 and incrementing learned clients.
		relay.ID = tailcfg.NodeID(1<<60 + i)
		relay.AllowedIPs = nil
		relay.Cap = tailcfg.CurrentCapabilityVersion
		copied = append(copied, relay)
	}
	b.peerRelayNodes = copied
	return nil
}

func (b *locoBackend) appendRelayNodes(nm *netmap.NetworkMap) {
	for _, relay := range b.peerRelayNodes {
		merged := false
		for index, existing := range nm.Peers {
			if existing.Key() != relay.Key {
				continue
			}
			node := existing.AsStruct().Clone()
			node.DiscoKey, node.HomeDERP = relay.DiscoKey, relay.HomeDERP
			if node.CapMap == nil {
				node.CapMap = make(tailcfg.NodeCapMap)
			}
			for capability, values := range relay.CapMap {
				node.CapMap[capability] = slices.Clone(values)
			}
			nm.Peers[index] = node.View()
			merged = true
			break
		}
		if merged {
			continue
		}
		nm.Peers = append(nm.Peers, relay.View())
	}
}

// setFilter installs packet and discovery capabilities together. Engine.SetFilter
// only updates the TUN packet filter; magicsock independently consumes RelayTarget
// capabilities when selecting relay servers and must receive the same snapshot.
func (b *locoBackend) setFilter(f *filter.Filter) {
	b.sys.Engine.Get().SetFilter(f)
	b.sys.MagicSock.Get().SetFilter(f)
}

func (b *locoBackend) relayFilterMatches() []filter.Match {
	matches := make([]filter.Match, 0, len(b.peerRelayNodes))
	for _, relay := range b.peerRelayNodes {
		matches = append(matches, filter.Match{Srcs: slices.Clone(relay.Addresses), Caps: []filtertype.CapMatch{{Dst: b.addrPrefix, Cap: peercap.RelayTarget}}})
	}
	return matches
}

// updateServerMapLocked publishes the admitted peer set; b.mu must be held.
func (b *locoBackend) updateServerMapLocked() {
	derpRegion := b.derpRegionIDLocked()
	nm := &netmap.NetworkMap{
		NodeKey: b.pub,
		SelfNode: (&tailcfg.Node{
			ID:         1,
			StableID:   "1",
			Name:       "endpoint.paperboat.",
			User:       100,
			Key:        b.pub,
			DiscoKey:   b.discoPublic(),
			Addresses:  []netip.Prefix{b.addrPrefix},
			AllowedIPs: b.selfAllowedIPs(),
			HomeDERP:   derpRegion,
			Cap:        tailcfg.CurrentCapabilityVersion,
		}).View(),
	}
	for _, n := range b.clients {
		nm.Peers = append(nm.Peers, n.View())
	}
	b.appendRelayNodes(nm)
	slices.SortFunc(nm.Peers, func(a, b tailcfg.NodeView) int {
		return cmp.Compare(a.ID(), b.ID())
	})
	b.nm = nm

	mc := b.sys.MagicSock.Get()
	mc.SetNetworkMap(nm.SelfNode, nm.Peers)
	b.sys.Netstack.Get().UpdateNetstackIPs(nm)

}

// tailcatULASet returns the allocated ULA range for packet-filter logging.
var tailcatULASet = sync.OnceValue(func() *netipx.IPSet {
	var b netipx.IPSetBuilder
	b.AddPrefix(tsaddr.TailscaleULARange())
	s, err := b.IPSet()
	if err != nil {
		panic(err)
	}
	return s
})

func newNetstack(logf logger.Logf, sys *tsd.System) (*netstack.Impl, error) {
	return netstack.Create(logf,
		sys.Tun.Get(),
		sys.Engine.Get(),
		sys.MagicSock.Get(),
		sys.Dialer.Get(),
		sys.DNSManager.Get(),
		sys.ProxyMapper(),
	)
}

// createEngine creates the wgengine.Engine with userspace networking.
func createEngine(logf logger.Logf, lb *locoBackend) (err error) {
	sys := &lb.sys
	conf := wgengine.Config{
		ListenPort:             0,
		NetMon:                 sys.NetMon.Get(),
		Dialer:                 sys.Dialer.Get(),
		SetSubsystem:           sys.Set,
		Metrics:                sys.UserMetricsRegistry(),
		HealthTracker:          sys.HealthTracker.Get(),
		EventBus:               sys.Bus.Get(),
		OnDERPRecv:             lb.onDERPRecv,
		DERPAppName:            "paperboat",
		DERPCarrierFactory:     lb.derpCarrierFactory,
		TestOnlyPacketListener: lb.testOnlyPacketListener,
	}
	// Both sides deterministically derive a separate disco key from their
	// node private key. The public disco key, unlike the node key, is safe to
	// expose in direct-path disco frames. Knowing our own disco private key
	// is what lets either side seal the
	// call-me-maybe messages that advertise our UDP endpoints (see
	// locoBackend.advertiseEndpoints).
	conf.ForceDiscoKey = discoPrivateForNode(lb.priv)
	netns.SetEnabled(false)
	e, err := wgengine.NewUserspaceEngine(logf, conf)
	if err != nil {
		logf("wgengine.NewUserspaceEngine(tun %q) error: %v", "userspace-networking", err)
		return err
	}
	sys.Set(e)
	sys.NetstackRouter.Set(true)
	return nil
}

func pfxOf(a netip.Addr) netip.Prefix {
	return netip.PrefixFrom(a, a.BitLen())
}
