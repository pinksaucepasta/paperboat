package tailnet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/netip"
	"sort"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/tailscale/tailcat"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-virtual-udp
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-virtual-udp
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authorized-virtual-udp
	"tailscale.com/wgengine/filter"
)

func (a *Authority) peersLocked() (map[key.NodePublic]netip.Addr, map[netip.Addr]string) {
	peers := make(map[key.NodePublic]netip.Addr)
	admitted := make(map[netip.Addr]string)
	for _, p := range a.current.Peers {
		pub, _ := publicKey(p.Identity.WireGuardPublicKey)
		address := netip.MustParseAddr(p.Identity.VirtualAddress)
		peers[pub] = address
		raw, _ := json.Marshal(p.Identity)
		admitted[address] = string(raw)
	}
	return peers, admitted
}
func (a *Authority) replaceLocked() error {
	allowed := make(map[string]NetworkBinding, len(a.current.Peers))
	for _, p := range a.current.Peers {
		allowed[p.Identity.EndpointID] = p.Identity
	}
	for id, lease := range a.clients {
		if allowed[id] != lease.peer {
			_ = lease.client.Close()
			if a.clientEngine != nil {
				a.clientEngine.InvalidateAuthorizedPeer(lease.node)
			}
			delete(a.clients, id)
		}
	}
	if a.clientEngine != nil {
		peers, _ := a.peersLocked()
		if err := a.clientEngine.ReplaceAllowedPeers(peers); err != nil {
			return err
		}
	}
	if a.server == nil {
		return nil
	}
	peers, admitted := a.peersLocked()
	a.server.mu.Lock()
	previous := a.server.admitted
	a.server.admitted = admitted
	var removed []*Packet
	for p := range a.server.flows {
		addr, _ := netip.ParseAddrPort(p.RemoteAddr().String())
		if admitted[addr.Addr()] == "" || previous[addr.Addr()] != admitted[addr.Addr()] {
			removed = append(removed, p)
		}
	}
	a.server.mu.Unlock()
	if err := a.server.server.ReplaceAllowedPeers(peers); err != nil {
		return err
	}
	for _, p := range removed {
		_ = p.Close()
	}
	return nil
}

// Listen opens the machine's verified application port. Empty configuration is
// a valid listening engine with no admitted peers. Rotation/expiry closes leases.
func (a *Authority) Listen(region *tailcfg.DERPRegion) (*UDPServer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.usableLocked() || a.current.Self.Role != "machine" || region == nil {
		return nil, ErrAuthority
	}
	if a.server != nil {
		a.server.mu.Lock()
		closed := a.server.closed
		a.server.mu.Unlock()
		if !closed {
			return a.server, nil
		}
		a.server = nil
	}
	peers, admitted := a.peersLocked()
	server := &tailcat.Server{OnRelayControl: a.relayControl, DERPCarrierFactory: a.relay.factory, PeerRelayNodes: a.relay.peerNodes, TestOnlyPacketListener: a.options.TestOnlyPacketListener, Key: a.private, DisablePresharedKey: true, LocalAddr: netip.MustParseAddr(a.current.Self.VirtualAddress), AllowedPeers: peers, Region: region, ServedUDPPorts: []filter.PortRange{{First: NetworkPort, Last: NetworkPort}}, Logf: func(string, ...any) {}}
	var err error
	a.server, err = listenUDP(server, NetworkPort, admitted)
	return a.server, err
}

// Descriptor derives a peer address from current signed network authority.
// Paperboat authority mode uses explicit peer admission, pinned QUIC identity
// and operation grants rather than Tailcat's optional address PSK.
func (a *Authority) Descriptor(peerID string) (tailcat.Addr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.usableLocked() || a.current.Self.Role != "cli" {
		return "", ErrAuthority
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID != peerID {
			continue
		}
		node, err := publicKey(peer.Identity.WireGuardPublicKey)
		disco, discoErr := derpquic.ParseDiscoKey(peer.Identity.DiscoPublicKey)
		if err != nil || discoErr != nil || disco.IsZero() {
			return "", ErrAuthority
		}
		a.relay.mu.Lock()
		regions := make([]*tailcfg.DERPRegion, 0, len(a.relay.nodes))
		for _, candidate := range a.relay.nodes {
			regions = append(regions, regionForNode(candidate))
		}
		a.relay.mu.Unlock()
		sort.Slice(regions, func(i, j int) bool { return regions[i].RegionID < regions[j].RegionID })
		if len(regions) == 0 {
			return "", ErrAuthority
		}
		return (&tailcat.ConnInfo{ServerPublic: tailcat.NodePublic{NodePublic: node}, ServerDiscoPublic: tailcat.DiscoPublic{DiscoPublic: disco}, Region: regions[:1]}).Addr(), nil
	}
	return "", ErrAdmission
}

// Client binds a connection descriptor to the exact signed peer key before any
// network work. All peer leases share one authority-mode Tailcat engine and
// one aggregate flow bound; revoking a peer closes only that peer's leases.
func (a *Authority) Client(descriptor tailcat.Addr, peerID string) (*UDPClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.usableLocked() || a.current.Self.Role != "cli" {
		return nil, ErrAuthority
	}
	info, err := tailcat.ParseAddr(descriptor)
	if err != nil {
		return nil, ErrAuthority
	}
	for _, p := range a.current.Peers {
		if p.Identity.EndpointID != peerID {
			continue
		}
		pub, _ := publicKey(p.Identity.WireGuardPublicKey)
		disco, discoErr := derpquic.ParseDiscoKey(p.Identity.DiscoPublicKey)
		if discoErr != nil || info.ServerPublic.NodePublic != pub || info.ServerDiscoPublic.DiscoPublic != disco {
			return nil, ErrAuthority
		}
		a.relay.mu.Lock()
		grant := a.relay.grants[a.relay.node.NodeID]
		a.relay.mu.Unlock()
		if grant.PeerRelay != nil {
			matched := false
			for _, relayPeer := range grant.Peers {
				if relayPeer.WireGuardPublicKey == p.Identity.WireGuardPublicKey {
					disco, err := derpquic.ParseDiscoKey(relayPeer.DiscoPublicKey)
					matched = err == nil && disco == info.ServerDiscoPublic.DiscoPublic
					break
				}
			}
			if !matched {
				return nil, ErrAuthority
			}
		}
		digest := sha256.Sum256([]byte(descriptor))
		if lease, ok := a.clients[peerID]; ok {
			lease.client.mu.Lock()
			closed := lease.client.closed
			lease.client.mu.Unlock()
			if !closed && lease.peer == p.Identity && lease.descriptor == digest {
				return lease.client, nil
			}
			_ = lease.client.Close()
			if a.clientEngine != nil {
				a.clientEngine.InvalidateAuthorizedPeer(lease.node)
			}
			delete(a.clients, peerID)
		}
		if a.clientEngine == nil {
			peers, _ := a.peersLocked()
			regions := append([]*tailcfg.DERPRegion(nil), info.Region...)
			a.relay.mu.Lock()
			for _, node := range a.relay.nodes {
				region := regionForNode(node)
				found := false
				for _, existing := range regions {
					found = found || existing.RegionID == region.RegionID
				}
				if !found {
					regions = append(regions, region)
				}
			}
			factory, peerNodes := a.relay.factory, append([]*tailcfg.Node(nil), a.relay.peerNodes...)
			a.relay.mu.Unlock()
			sort.Slice(regions, func(i, j int) bool { return regions[i].RegionID < regions[j].RegionID })
			if len(regions) == 0 {
				return nil, ErrAuthority
			}
			a.clientEngine = &tailcat.Server{OnRelayControl: a.relayControl, DERPCarrierFactory: factory, PeerRelayNodes: peerNodes, TestOnlyPacketListener: a.options.TestOnlyPacketListener, Key: a.private, DisablePresharedKey: true, LocalAddr: netip.MustParseAddr(a.current.Self.VirtualAddress), AllowedPeers: peers, Region: regions[0], Logf: func(string, ...any) {}}
			if err := a.clientEngine.Start(); err != nil {
				a.clientEngine = nil
				return nil, err
			}
			if len(regions) > 1 {
				if err := a.clientEngine.SetRelayRegions(regions); err != nil {
					a.clientEngine.Close()
					a.clientEngine = nil
					return nil, err
				}
			}
			a.clientSlots = make(chan struct{}, MaxFlows)
			a.clients = make(map[string]authorizedClient)
		}
		ctx, cancel := context.WithCancel(context.Background())
		peerAddr := netip.AddrPortFrom(netip.MustParseAddr(p.Identity.VirtualAddress), NetworkPort)
		client := &UDPClient{dial: func(ctx context.Context) (tailcat.ConnPacketConn, error) {
			return a.clientEngine.DialAuthorizedUDP(ctx, pub, disco, peerAddr)
		}, slots: a.clientSlots, port: NetworkPort, flows: make(map[*Packet]struct{}), ctx: ctx, cancel: cancel}
		a.clients[peerID] = authorizedClient{client: client, peer: p.Identity, descriptor: digest, node: pub}
		return client, nil
	}
	return nil, ErrAdmission
}

// InvalidateClient closes the exact cached client after its transport has
// demonstrably failed. Pointer matching prevents a late failure from closing
// a replacement created by another reconnect attempt.
func (a *Authority) InvalidateClient(client *UDPClient) bool {
	if client == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, lease := range a.clients {
		if lease.client != client {
			continue
		}
		_ = lease.client.Close()
		if a.clientEngine != nil {
			a.clientEngine.InvalidateAuthorizedPeer(lease.node)
		}
		delete(a.clients, id)
		return true
	}
	return false
}
