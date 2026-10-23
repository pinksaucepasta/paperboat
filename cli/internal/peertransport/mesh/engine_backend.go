// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"slices"
	"sync"

	"go4.org/netipx"
	"tailscale.com/disco"
	"tailscale.com/net/dns"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tailcfg"
	"tailscale.com/tailcfg/peercap"
	"tailscale.com/tsd"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/types/nettype"
	"tailscale.com/util/mak"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/filter"
	"tailscale.com/wgengine/filter/filtertype"
	"tailscale.com/wgengine/magicsock"
	"tailscale.com/wgengine/netstack"
	"tailscale.com/wgengine/router"
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
	stunServers            []string
	logf                   logger.Logf
	allowedPeers           map[key.NodePublic]netip.Addr
	policyMu               sync.Mutex // serializes policy replacement through machine synchronization
	nextClientID           tailcfg.NodeID

	// discoPublic returns the node's disco public key, memoized to
	// avoid redoing the curve25519 derivation for every client that
	// joins.
	discoPublic func() key.DiscoPublic

	// onDERPRecv is called for non-disco DERP packets before the
	// peer map lookup. Set before createEngine.
	onDERPRecv func(regionID tailcfg.DERPRegionID, src key.NodePublic, pkt []byte) bool

	mu      sync.Mutex
	clients map[key.NodePublic]*tailcfg.Node // for the server
	// clientByAddr indexes clients by address for the per-packet peer lookup; it changes only
	// through setClientLocked and deleteClientLocked.
	clientByAddr map[netip.Addr]key.NodePublic
	nm           *netmap.NetworkMap
	eps          []netip.AddrPort // our current local UDP endpoints, sorted
	closeOnce    sync.Once
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
	if b.homeDERP != 0 && b.dm.Regions[b.homeDERP].HasDERP() {
		return b.homeDERP
	}
	for _, r := range b.dm.Regions {
		if r.HasDERP() {
			return r.RegionID
		}
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
// It runs for every outbound packet, so it is one map lookup, not a scan of the clients.
func (b *locoBackend) peerByIP(dst netip.Addr) (_ key.NodePublic, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k, ok := b.clientByAddr[dst]
	return k, ok
}

// setClientLocked records an admitted client and indexes its addresses; b.mu must be held.
func (b *locoBackend) setClientLocked(k key.NodePublic, n *tailcfg.Node) {
	b.deleteClientLocked(k)
	mak.Set(&b.clients, k, n)
	for _, p := range n.Addresses {
		mak.Set(&b.clientByAddr, p.Addr(), k)
	}
}

// deleteClientLocked removes a client and its address index entries; b.mu must be held.
func (b *locoBackend) deleteClientLocked(k key.NodePublic) {
	if n := b.clients[k]; n != nil {
		for _, p := range n.Addresses {
			if b.clientByAddr[p.Addr()] == k {
				delete(b.clientByAddr, p.Addr())
			}
		}
	}
	delete(b.clients, k)
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
func (b *locoBackend) advertiseEndpoints() { b.advertiseEndpointsTo(key.NodePublic{}) }

// advertiseEndpointsTo sends our endpoints to one peer, or to every peer when only is zero. A peer
// starting discovery (its meow and our acknowledgment) needs them alone: advertising to every peer on
// each meow made bringing up N peers cost O(N²) sealed messages.
func (b *locoBackend) advertiseEndpointsTo(only key.NodePublic) {
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
	targets := endpointTargets(peers, only, b.derpRegionIDLocked())
	b.mu.Unlock()
	if len(eps) == 0 || len(targets) == 0 {
		return
	}
	payload := (&disco.CallMeMaybe{MyNumber: eps}).AppendMarshal(nil)
	discoPriv := discoPrivateForNode(b.priv)
	mc := b.sys.MagicSock.Get()
	for _, p := range targets {
		// Frame and seal the message the same way magicsock's
		// sendDiscoMessage does, so the peer's stock magicsock
		// processes it natively. Use that peer's current rendezvous after
		// regional promotion, as the meow handshake does.
		pkt := make([]byte, 0, 512)
		pkt = append(pkt, disco.Magic...)
		pkt = b.discoPublic().AppendTo(pkt)
		pkt = append(pkt, discoPriv.Shared(p.disco).Seal(payload)...)
		if _, err := mc.SendDERPPacketTo(p.key, p.region, pkt); err != nil {
			b.logf("advertiseEndpoints to %v: %v", p.key.ShortString(), err)
		}
	}
}

type endpointTarget struct {
	key    key.NodePublic
	disco  key.DiscoPublic
	region tailcfg.DERPRegionID
}

// endpointTargets lists the peers to advertise endpoints to (only that peer when only is set) with each
// peer's rendezvous: its home relay, or ours when it has none. One pass over the peers.
func endpointTargets(peers []tailcfg.NodeView, only key.NodePublic, fallback tailcfg.DERPRegionID) []endpointTarget {
	var targets []endpointTarget
	for _, p := range peers {
		if !only.IsZero() && p.Key() != only {
			continue
		}
		region := p.HomeDERP()
		if region == 0 {
			region = fallback
		}
		targets = append(targets, endpointTarget{key: p.Key(), disco: p.DiscoKey(), region: region})
	}
	return targets
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
	b.setClientLocked(src, &tailcfg.Node{
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

	// No engine reconfig needed: the WireGuard machine learns about the
	// new peer lazily via the config source installed with
	// SetPeerConfigFunc when the client's handshake arrives.

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
