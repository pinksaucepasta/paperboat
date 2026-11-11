// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
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
	TestOnlyPacketListener nettype.PacketListener
	PeerRelayNodes         []*tailcfg.Node
	RelayControlPeers      []key.NodePublic
	OnRelayControl         func(tailcfg.DERPRegionID, key.NodePublic, []byte) bool
	DERPCarrierFactory     magicsock.DERPCarrierFactory
	Key                    key.NodePrivate
	DiscoPrivate           key.DiscoPrivate
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
	peerReady      map[key.NodePublic]*meowWait
	relayConnects  relayConnects
}

// meowWait is the outstanding handshake with one peer, shared by concurrent dials: ready closes on its answer.
type meowWait struct {
	ready   chan struct{}
	nonce   [meowNonceLen]byte
	claimed bool // an answer is being taken; peerReadyMu guards it
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
	observe := &underlayDiagnostics{engineLabel: underlayLabel(s.Key.Public().String())}
	baseLogf := logf
	logf = func(format string, args ...any) { observe.record(format, args...); baseLogf(format, args...) }
	priv := s.Key
	reg := s.Region
	if reg != nil && reg.RegionID == 0 {
		return fmt.Errorf("missing RegionID in %v", logger.AsJSON(reg))
	}

	lb := newLocoBackend(priv)
	if !s.DiscoPrivate.IsZero() {
		lb.discoPrivate = s.DiscoPrivate
		lb.discoPublic = sync.OnceValue(func() key.DiscoPublic { return lb.discoPrivate.Public() })
	}
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
			nonce, echoed := meowedNonce(pkt) // pkt is the receiver's buffer: read it before returning
			go s.meowed(src, regionID, nonce, echoed)
			return true
		}
		if _, discoPub, nonce, ok := ParseMeowPing(pkt); ok {
			mc := lb.sys.MagicSock.Get()
			go func() {
				// Only reply once the client is fully added as a peer:
				// "meowed" is the ack that tells the client it can
				// start dialing. Disallowed clients get no reply.
				if lb.onMeow(src, discoPub) {
					// The peer reached us through regionID, so it is a rendezvous both ends hold: answer it there
					// rather than on the peer's default region, which may be a relay this engine cannot reach
					// (pbmac 2026-11-05). Tailscale replies on the region it heard a peer from (magicsock
					// derp.go derpRoute, issue 150).
					_ = lb.setPeerRelayRegion(src, regionID)
					if _, err := mc.SendDERPPacketTo(src, regionID, EncodeMeowed(nonce)); err == nil {
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
		return func(c nettype.ConnPacketConn) { h(newIdlePacketConn(newStallBoundedConn(c), s.udpIdleTimeout())) }, true
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
	conn, err := s.lb.ns.DialContextUDPWithBind(ctx, s.lb.addr, ap)
	if err != nil {
		return nil, err
	}
	return newStallBoundedConn(conn), nil
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

// meowed handles a peer's answer to our handshake. Only the first answer to the outstanding handshake (its nonce)
// records the region it arrived on: the peer is homed there, so send to it there from now on, as magicsock learns a
// peer's derpRoute from where it was heard (derp.go addDerpPeerRoute). A duplicate, a late answer to an earlier
// handshake, or a second answer from a peer still on two relays moves nothing. A bare answer (a peer that sends no
// nonce) completes the handshake without moving the peer. The region is recorded before the dial is released, so
// its first packets take it.
func (s *Server) meowed(src key.NodePublic, regionID tailcfg.DERPRegionID, nonce [meowNonceLen]byte, echoed bool) {
	// The waiter is claimed under peerReadyMu and the region set without it: setPeerRelayRegion takes policyMu, which
	// SetPeerContacts and InvalidateAuthorizedPeer hold while they take peerReadyMu (ABBA, review of b6eb2018c).
	s.peerReadyMu.Lock()
	w := s.peerReady[src]
	if w == nil || w.claimed || (echoed && nonce != w.nonce) {
		s.peerReadyMu.Unlock()
		return
	}
	w.claimed = true
	s.peerReadyMu.Unlock()
	if echoed {
		_ = s.lb.setPeerRelayRegion(src, regionID)
	}
	s.peerReadyMu.Lock()
	defer s.peerReadyMu.Unlock()
	if s.peerReady[src] == w { // not invalidated meanwhile
		close(w.ready)
	}
}

// meowNonceRead fills a handshake nonce; tests replace it to fail.
var meowNonceRead = rand.Read

// meowFanout reports whether an unanswered handshake also asks on the other relays at this attempt: on attempts 1,
// 2, 4, 8 and 16 (a per-peer exponential backoff), so a long dial to an unreachable peer probes five times, not
// once a second.
func meowFanout(attempt int) bool { return attempt > 0 && attempt&(attempt-1) == 0 && attempt <= 16 }

// awaitMeow sends the meow handshake over the peer's relay region until the peer acknowledges it.
func (s *Server) awaitMeow(ctx context.Context, peer key.NodePublic) error {
	s.peerReadyMu.Lock()
	if s.peerReady == nil {
		s.peerReady = make(map[key.NodePublic]*meowWait)
	}
	w := s.peerReady[peer]
	if w == nil {
		w = &meowWait{ready: make(chan struct{})}
		if _, err := meowNonceRead(w.nonce[:]); err != nil {
			// No handshake without a nonce: a zero one would let any stale answer complete it.
			s.peerReadyMu.Unlock()
			return fmt.Errorf("mesh: handshake nonce: %w", err)
		}
		s.peerReady[peer] = w
	}
	s.peerReadyMu.Unlock()
	mc := s.lb.sys.MagicSock.Get()
	packet := EncodeMeowPing(s.lb.pub, mc.DiscoPublicKey(), w.nonce)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// send delivers the handshake through region, connecting to it first when this engine has no carrier there: a
	// peer's home is reached where it is, as magicsock opens a peer's home DERP on demand (derp.go
	// derpWriteChanForRegion). Connects are shared by every handshake on this engine (relayConnects).
	send := func(region tailcfg.DERPRegionID) error {
		if err := s.relayConnects.ensure(ctx, region, s.lb.connectRelay); err != nil {
			return err
		}
		return mc.SendDERPPacketToRegion(peer, region, packet)
	}
	var lastSendErr error
	for attempt := 0; ; attempt++ {
		// Regional recovery may promote a different rendezvous while bootstrap is
		// waiting for admission. Retry on the current route, not the initial one.
		region := s.lb.peerDERPRegion(peer)
		lastSendErr = send(region)
		diagnosticlog.TryInfo("native underlay session", "stage", "meow_send", "failed", lastSendErr != nil)
		if meowFanout(attempt) {
			// Unanswered where the peer was last known: it may have moved to another relay before its new home
			// reached us. Ask on every other relay (at most maxRelayRegions); the answer comes back on the peer's
			// home and is recorded there.
			var probes sync.WaitGroup
			var probed, failed atomic.Int32
			for _, other := range s.lb.relayRegionIDs() {
				if other != region {
					probed.Add(1)
					probes.Go(func() {
						if send(other) != nil {
							failed.Add(1)
						}
					})
				}
			}
			probes.Wait() // each send is bounded by relayConnectTimeout
			diagnosticlog.TryInfo("native underlay session", "stage", "meow_fanout", "attempt", attempt, "regions", probed.Load(), "failed", failed.Load())
		}
		select {
		case <-w.ready:
			diagnosticlog.TryInfo("native underlay session", "stage", "meow_acked")
			// The first advertisement in onMeow can arrive before the remote
			// has admitted us. Its acknowledgment proves that admission is now
			// installed, so send our candidates again for direct discovery.
			s.lb.advertiseEndpointsTo(peer)
			return nil
		case <-ctx.Done():
			s.peerReadyMu.Lock()
			if s.peerReady[peer] == w {
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
	if s.lb == nil {
		return
	}
	s.lb.policyMu.Lock()
	defer s.lb.policyMu.Unlock()
	s.invalidateAuthorizedPeerLocked(peer)
}

// invalidateAuthorizedPeerLocked requires the backend policy lock.
func (s *Server) invalidateAuthorizedPeerLocked(peer key.NodePublic) {
	s.peerReadyMu.Lock()
	delete(s.peerReady, peer)
	s.peerReadyMu.Unlock()
	b := s.lb
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

// listenPort is the engine's fixed UDP port (stable for NAT mappings); tests set 0 so parallel engines share nothing.
var listenPort uint16 = 41642

// createEngine creates the wgengine.Engine with userspace networking.
func createEngine(logf logger.Logf, lb *locoBackend) (err error) {
	sys := &lb.sys
	conf := wgengine.Config{
		ListenPort:             listenPort,
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
	conf.ForceDiscoKey = lb.discoPrivate
	netns.SetEnabled(false)
	e, err := wgengine.NewUserspaceEngine(logf, conf)
	if err != nil {
		logf("wgengine.NewUserspaceEngine(tun %q) error: %v", "userspace-networking", err)
		return err
	}
	e.SetPeerSessionStateFunc(func(peer key.NodePublic, state wgengine.PeerWireGuardState) {
		diagnosticlog.TryInfo("native underlay session", "engine_label", underlayLabel(lb.pub.String()), "peer_label", underlayLabel(peer.String()), "state", int(state), "observed_unix_us", time.Now().UnixMicro())
	})
	sys.Set(e)
	sys.NetstackRouter.Set(true)
	return nil
}

func pfxOf(a netip.Addr) netip.Prefix {
	return netip.PrefixFrom(a, a.BitLen())
}
