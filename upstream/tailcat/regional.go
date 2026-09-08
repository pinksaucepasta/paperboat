package tailcat

import (
	"context"
	"errors"
	"fmt"

	"go4.org/netipx"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
	"tailscale.com/wgengine/filter"
)

const maxRelayRegions = 32

func cloneRelayRegions(regions []*tailcfg.DERPRegion) ([]*tailcfg.DERPRegion, error) {
	if len(regions) > maxRelayRegions {
		return nil, fmt.Errorf("tailcat: relay regions must contain at most %d entries", maxRelayRegions)
	}
	seen := make(map[tailcfg.DERPRegionID]bool, len(regions))
	out := make([]*tailcfg.DERPRegion, len(regions))
	for i, region := range regions {
		if region == nil || region.RegionID < 1 || region.RegionID > 65535 || seen[region.RegionID] {
			return nil, errors.New("tailcat: relay region IDs must be unique and in 1..65535")
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
	if s.lb == nil {
		if len(cloned) == 0 {
			return errors.New("tailcat: cannot start without a relay region")
		}
		s.relayRegions, s.Region, s.RegionID = cloned, cloned[0], cloned[0].RegionID
		return nil
	}
	s.lb.setRelayRegions(cloned)
	s.relayRegions = cloned
	if len(cloned) != 0 {
		s.Region, s.RegionID = cloned[0], cloned[0].RegionID
	}
	return nil
}

// SetRelayRegions replaces the client's relay inventory. It may be called
// before or after StartNetwork.
func (c *Client) SetRelayRegions(regions []*tailcfg.DERPRegion) error {
	cloned, err := cloneRelayRegions(regions)
	if err != nil {
		return err
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.relayRegions = cloned
	if c.started {
		c.lb.setRelayRegions(cloned)
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
	if b.serverPub.IsZero() {
		b.updateServerMapLocked()
	} else {
		b.updateClientHomeLocked()
	}
	b.mu.Unlock()
	b.sys.MagicSock.Get().SetDERPMap(dm)
}

func (b *locoBackend) updateClientHomeLocked() {
	self := b.nm.SelfNode.AsStruct().Clone()
	self.HomeDERP = b.homeDERP
	peers := make([]tailcfg.NodeView, 0, len(b.nm.Peers))
	for _, pv := range b.nm.Peers {
		p := pv.AsStruct().Clone()
		peers = append(peers, p.View())
	}
	b.nm = &netmap.NetworkMap{NodeKey: b.pub, SelfNode: self.View(), Peers: peers}
	b.sys.MagicSock.Get().SetNetworkMap(b.nm.SelfNode, b.nm.Peers)
	b.sys.Netstack.Get().UpdateNetstackIPs(b.nm)
}

// SetPeerRelayNodes atomically replaces discovery-only peer relay metadata.
func (s *Server) SetPeerRelayNodes(nodes []*tailcfg.Node) error {
	if s.lb == nil {
		s.PeerRelayNodes = nodes
		return nil
	}
	return s.lb.replacePeerRelayNodes(nodes, func() *filter.Filter { return s.buildFilterWithState(s.lb.sys.Engine.Get().GetFilter()) })
}

func (c *Client) SetPeerRelayNodes(nodes []*tailcfg.Node) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.lb == nil {
		c.PeerRelayNodes = nodes
		return nil
	}
	return c.lb.replacePeerRelayNodes(nodes, func() *filter.Filter {
		localNets := new(netipx.IPSetBuilder)
		localNets.AddPrefix(c.lb.addrPrefix)
		local, _ := localNets.IPSet()
		return filter.New(c.lb.relayFilterMatches(), nil, local, tailcatULASet(), c.lb.sys.Engine.Get().GetFilter(), c.lb.logf)
	})
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
		return errors.New("tailcat: server is not started")
	}
	if !s.lb.onMeow(peer, disco) {
		return errors.New("tailcat: peer is not admitted")
	}
	return nil
}

// PrepareRelay proves that the selected region's owned carrier can connect and ping.
func (s *Server) PrepareRelay(ctx context.Context, id tailcfg.DERPRegionID) error {
	if s.lb == nil {
		return errors.New("tailcat: server is not started")
	}
	return s.lb.sys.MagicSock.Get().PrepareDERP(ctx, id)
}

func (c *Client) PrepareRelay(ctx context.Context, id tailcfg.DERPRegionID) error {
	if err := c.ensureStarted(ctx); err != nil {
		return err
	}
	return c.lb.sys.MagicSock.Get().PrepareDERP(ctx, id)
}

// StartNetwork starts the client network without performing the meow handshake.
func (c *Client) StartNetwork(ctx context.Context) error { return c.ensureStarted(ctx) }

func (s *Server) SetPeerRelayRegion(peer key.NodePublic, id tailcfg.DERPRegionID) error {
	if s.lb == nil {
		return errors.New("tailcat: server is not started")
	}
	return s.lb.setPeerRelayRegion(peer, id)
}

func (c *Client) SetPeerRelayRegion(peer key.NodePublic, id tailcfg.DERPRegionID) error {
	if c.lb == nil || !c.started {
		return errors.New("tailcat: client is not started")
	}
	return c.lb.setPeerRelayRegion(peer, id)
}

func (b *locoBackend) setPeerRelayRegion(peer key.NodePublic, id tailcfg.DERPRegionID) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dm.Regions[id] == nil {
		return errors.New("tailcat: unknown relay region")
	}
	if b.serverPub.IsZero() {
		n := b.clients[peer]
		if n == nil {
			return errors.New("tailcat: peer is not admitted")
		}
		n = n.Clone()
		n.HomeDERP = id
		b.clients[peer] = n
		b.updateServerMapLocked()
		return nil
	}
	if peer != b.serverPub {
		return errors.New("tailcat: peer is not admitted")
	}
	self := b.nm.SelfNode
	peers := make([]tailcfg.NodeView, 0, len(b.nm.Peers))
	for _, pv := range b.nm.Peers {
		p := pv.AsStruct().Clone()
		if p.Key == peer {
			p.HomeDERP = id
		}
		peers = append(peers, p.View())
	}
	b.nm = &netmap.NetworkMap{NodeKey: b.pub, SelfNode: self, Peers: peers}
	b.sys.MagicSock.Get().SetNetworkMap(self, peers)
	b.sys.Netstack.Get().UpdateNetstackIPs(b.nm)
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
	if b.serverPub.IsZero() {
		if b.authority {
			return b.allowedPeers[peer].IsValid()
		}
		_, ok := b.clients[peer]
		return ok
	}
	return peer == b.serverPub
}

func (b *locoBackend) setRelayControlPeers(peers []key.NodePublic) error {
	if len(peers) > 16 {
		return errors.New("tailcat: too many relay control peers")
	}
	next := make(map[key.NodePublic]bool, len(peers))
	for _, peer := range peers {
		if peer.IsZero() || peer == b.pub || next[peer] {
			return errors.New("tailcat: invalid relay control peer")
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

func (c *Client) SetRelayControlPeers(peers []key.NodePublic) error {
	if c.lb == nil {
		c.RelayControlPeers = append([]key.NodePublic(nil), peers...)
		return nil
	}
	c.lb.mu.Lock()
	defer c.lb.mu.Unlock()
	return c.lb.setRelayControlPeers(peers)
}

func (s *Server) SendRelayControl(peer key.NodePublic, id tailcfg.DERPRegionID, payload []byte) error {
	if s.lb == nil {
		return errors.New("tailcat: server is not started")
	}
	if !s.lb.relayPeerAdmitted(peer) {
		return errors.New("tailcat: peer is not admitted")
	}
	return s.lb.sys.MagicSock.Get().SendDERPPacketToRegion(peer, id, payload)
}

func (c *Client) SendRelayControl(peer key.NodePublic, id tailcfg.DERPRegionID, payload []byte) error {
	if c.lb == nil || !c.started {
		return errors.New("tailcat: client is not started")
	}
	if !c.lb.relayPeerAdmitted(peer) {
		return errors.New("tailcat: peer is not admitted")
	}
	return c.lb.sys.MagicSock.Get().SendDERPPacketToRegion(peer, id, payload)
}
