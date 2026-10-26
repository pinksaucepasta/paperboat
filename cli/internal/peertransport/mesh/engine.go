// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"sync"
	"time"

	"go4.org/netipx"
	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/net/tsdial"
	"tailscale.com/tailcfg"
	"tailscale.com/tsd"
	"tailscale.com/types/ipproto"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
	"tailscale.com/types/views"
	"tailscale.com/util/eventbus"
	"tailscale.com/util/mak"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/filter"
	"tailscale.com/wgengine/magicsock"
	"tailscale.com/wgengine/netstack"
)

// Server owns one userspace WireGuard engine for an explicitly authorized endpoint.
// Set configuration before Start. Live policy changes use the replacement methods.
// An empty peer map denies all peers; a zero identity or missing allocation is invalid.
type Server struct {
	// usagePaths caches each peer's accounting path for usagePathTTL.
	usagePaths             usagePathCache
	STUNServers            []string
	TestOnlyPacketListener nettype.PacketListener
	PeerRelayNodes         []*tailcfg.Node
	RelayControlPeers      []key.NodePublic
	OnRelayControl         func(tailcfg.DERPRegionID, key.NodePublic, []byte) bool
	DERPCarrierFactory     magicsock.DERPCarrierFactory
	Key                    key.NodePrivate
	LocalAddr              netip.Addr
	AllowedPeers           map[key.NodePublic]netip.Addr
	// PeerContacts are the allowed peers' direct contacts from the signed configuration, applied at Start
	// (SetPeerContacts replaces them later).
	PeerContacts map[key.NodePublic]PeerContact
	// OnLocalEndpoints, when set, is called (on its own goroutine) each time the engine's own candidate
	// addresses change, so they can be reported.
	OnLocalEndpoints func()
	Logf             logger.Logf
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
	if err := ValidateSTUNServers(s.STUNServers); err != nil {
		return err
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
	lb.onLocalEndpoints = s.OnLocalEndpoints
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
	lb.stunServers = append([]string(nil), s.STUNServers...)
	lb.dm = withSTUNServers(lb.dm, lb.stunServers)
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
					if _, err := mc.SendDERPPacketTo(src, regionID, EncodeMeowed()); err == nil {
						// The client's peer map is installed before it sends the meow.
						// Advertise after the acknowledgment on the same DERP route so
						// it can process our endpoints before probing directly.
						lb.advertiseEndpointsTo(src)
					}
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
	if len(s.PeerContacts) > 0 {
		return s.SetPeerContacts(s.PeerContacts)
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
	if s.lb.directContact(peer) {
		return s.awaitRelayOrDirect(ctx, peer)
	}
	return s.awaitMeow(ctx, peer)
}

// awaitMeow sends the meow handshake over the peer's relay region until the peer acknowledges it.
func (s *Server) awaitMeow(ctx context.Context, peer key.NodePublic) error {
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
			// The first advertisement in onMeow can arrive before the remote
			// has admitted us. Its acknowledgment proves that admission is now
			// installed, so send our candidates again for direct discovery.
			s.lb.advertiseEndpointsTo(peer)
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
		b.deleteClientLocked(peer)
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

// TailcatAddr returns the tailcat address that clients use to connect to this
// server. It embeds the full DERP region, so clients don't need to
// fetch the DERP map from the network. It must only be called after
// [Server.Start].
func (s *Server) TailcatAddr() Addr {
	return s.lb.tailcatAddr()
}

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
		ListenPort:             41642,
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
