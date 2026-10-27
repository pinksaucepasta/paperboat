package tailnet

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/netip"
	"sort"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
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
	for _, p := range a.installedPeersLocked() {
		pub, _ := publicKey(p.Identity.WireGuardPublicKey)
		address := netip.MustParseAddr(p.Identity.VirtualAddress)
		peers[pub] = address
		// Raw UDP fences include scopes. Native owners revoke individual
		// authorized streams; their shared carrier fences only peer identity.
		var fence any = p
		if a.streamScopes {
			fence = p.Identity
		}
		raw, _ := json.Marshal(fence)
		admitted[address] = string(raw)
	}
	return peers, admitted
}

// peerContactsLocked is each peer's direct contact from the signed configuration: its disco key and the
// endpoints the server delivered (PLAN 3b "Direct paths without a relay"). Peers without endpoints have none.
func (a *Authority) peerContactsLocked() map[key.NodePublic]mesh.PeerContact {
	contacts := make(map[key.NodePublic]mesh.PeerContact)
	for _, p := range a.installedPeersLocked() {
		if len(p.Endpoints) == 0 {
			continue
		}
		pub, err := publicKey(p.Identity.WireGuardPublicKey)
		disco, discoErr := derpquic.ParseDiscoKey(p.Identity.DiscoPublicKey)
		if err != nil || discoErr != nil || disco.IsZero() {
			continue
		}
		endpoints := make([]netip.AddrPort, 0, len(p.Endpoints))
		for _, raw := range p.Endpoints {
			endpoints = append(endpoints, netip.MustParseAddrPort(raw)) // validated with the configuration
		}
		contacts[pub] = mesh.PeerContact{Disco: disco, Endpoints: endpoints}
	}
	return contacts
}

// PeerHasEndpoints reports whether the signed configuration gives endpointID's own addresses, so a native
// dial can reach it directly when no relay is usable.
func (a *Authority) PeerHasEndpoints(endpointID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == nil {
		return false
	}
	for _, p := range a.current.Peers {
		if p.Identity.EndpointID == endpointID {
			return len(p.Endpoints) > 0
		}
	}
	return false
}

// localAddresses lists this host's interface addresses; a variable so tests can supply their own.
var localAddresses = func() []netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if prefix, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, prefix.Addr().Unmap())
		}
	}
	return out
}

// PeerIsLocal reports whether the peer runs on this host: one of its signed candidate endpoints is an address
// of a local interface (the daemon reaching its own machine's host runtime, e.g. inspecting a local preview).
func (a *Authority) PeerIsLocal(endpointID string) bool {
	a.mu.Lock()
	var endpoints []string
	if a.current != nil {
		for _, p := range a.current.Peers {
			if p.Identity.EndpointID == endpointID {
				endpoints = p.Endpoints
			}
		}
	}
	a.mu.Unlock()
	if len(endpoints) == 0 {
		return false
	}
	local := localAddresses()
	for _, endpoint := range endpoints {
		addrPort, err := netip.ParseAddrPort(endpoint)
		if err != nil {
			continue
		}
		for _, l := range local {
			if l == addrPort.Addr().Unmap() {
				return true
			}
		}
	}
	return false
}

// PeerPath reports the selected WireGuard underlay only for a currently
// authorized peer. It returns unknown while the peer is idle or changing paths.
func (a *Authority) PeerPath(endpointID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.usableLocked() {
		return "", ErrAuthority
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID != endpointID {
			continue
		}
		public, err := publicKey(peer.Identity.WireGuardPublicKey)
		if err != nil {
			return "", ErrAuthority
		}
		if a.clientEngine != nil {
			return a.clientEngine.PeerPath(public), nil
		}
		if a.server != nil && a.server.server != nil {
			return a.server.server.PeerPath(public), nil
		}
		return "unknown", nil
	}
	return "", ErrAdmission
}

// PeerPaths reports the current data path (direct, peer_relay or regional_relay) to every admitted peer
// with an active connection, by endpoint ID, from one engine status (F21-06). Idle peers are left out.
func (a *Authority) PeerPaths() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	paths := map[string]string{}
	if !a.usableLocked() {
		return paths
	}
	engine := a.clientEngine
	if engine == nil && a.server != nil {
		engine = a.server.server
	}
	if engine == nil {
		return paths
	}
	endpoints := make(map[key.NodePublic]string, len(a.current.Peers))
	keys := make([]key.NodePublic, 0, len(a.current.Peers))
	for _, peer := range a.current.Peers {
		if public, err := publicKey(peer.Identity.WireGuardPublicKey); err == nil {
			endpoints[public] = peer.Identity.EndpointID
			keys = append(keys, public)
		}
	}
	for public, path := range engine.PeerPaths(keys) {
		paths[endpoints[public]] = path
	}
	return paths
}

func (a *Authority) replaceLocked() error {
	a.relay.mu.Lock()
	a.relay.updateUsagePeersLocked(a.current.Peers)
	a.relay.mu.Unlock()
	type admittedPeer struct {
		binding NetworkBinding
		scopes  []NetworkScope
	}
	allowed := make(map[string]admittedPeer, len(a.current.Peers))
	for _, p := range a.current.Peers {
		allowed[p.Identity.EndpointID] = admittedPeer{binding: p.Identity, scopes: p.Scopes}
	}
	for id, lease := range a.clients {
		peer := allowed[id]
		if peer.binding != lease.peer || !a.streamScopes && !scopesRetained(lease.scopes, peer.scopes) {
			_ = lease.client.Close()
			if a.clientEngine != nil {
				a.clientEngine.InvalidateAuthorizedPeer(lease.node)
			}
			delete(a.clients, id)
		} else {
			lease.scopes = append([]NetworkScope(nil), peer.scopes...)
			a.clients[id] = lease
		}
	}
	contacts := a.peerContactsLocked()
	if a.clientEngine != nil {
		peers, _ := a.peersLocked()
		if err := a.clientEngine.ReplaceAllowedPeers(peers); err != nil {
			return err
		}
		if err := a.clientEngine.SetPeerContacts(contacts); err != nil {
			return err
		}
	}
	if a.server == nil {
		a.notifyScopeChangesLocked()
		return nil
	}
	peers, admitted := a.peersLocked()
	a.server.mu.Lock()
	previous := a.server.admitted
	for address, oldFence := range previous {
		newFence := admitted[address]
		if newFence == "" || oldFence == newFence {
			continue
		}
		var oldPeer, newPeer NetworkPeer
		if !a.streamScopes && json.Unmarshal([]byte(oldFence), &oldPeer) == nil && json.Unmarshal([]byte(newFence), &newPeer) == nil && oldPeer.Identity == newPeer.Identity && scopesRetained(oldPeer.Scopes, newPeer.Scopes) {
			admitted[address] = oldFence
		}
	}
	a.server.admitted = admitted
	a.server.admission = a.peerAdmissionExpiriesLocked()
	var removed []*Packet
	for p := range a.server.flows {
		remote := p.RemoteAddr()
		if remote == nil {
			removed = append(removed, p)
			continue
		}
		addr, _ := netip.ParseAddrPort(remote.String())
		if admitted[addr.Addr()] == "" || previous[addr.Addr()] != admitted[addr.Addr()] {
			removed = append(removed, p)
		}
	}
	a.server.mu.Unlock()
	if err := a.server.server.ReplaceAllowedPeers(peers); err != nil {
		return err
	}
	if err := a.server.server.SetPeerContacts(contacts); err != nil {
		return err
	}
	for _, p := range removed {
		_ = p.Close()
	}
	a.notifyScopeChangesLocked()
	return nil
}

// Listen opens the machine's verified application port. Empty configuration is
// a valid listening engine with no admitted peers. Rotation/expiry closes leases.
func (a *Authority) Listen(region *tailcfg.DERPRegion) (*UDPServer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.usableLocked() || a.current.Self.Role != "machine" {
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
	server := &mesh.Server{OnRelayControl: a.relayControl, DERPCarrierFactory: a.relay.factory, PeerRelayNodes: a.relay.peerNodes, RelayControlPeers: a.relay.controlPeers, TestOnlyPacketListener: a.options.TestOnlyPacketListener, Key: a.private, LocalAddr: netip.MustParseAddr(a.current.Self.VirtualAddress), AllowedPeers: peers, PeerContacts: a.peerContactsLocked(), OnLocalEndpoints: a.endpointsChanged, Region: region, ServedUDPPorts: []filter.PortRange{{First: NetworkPort, Last: NetworkPort}}, Logf: func(string, ...any) {}}
	var err error
	a.server, err = listenUDP(server, NetworkPort, admitted, a.peerAdmissionExpiriesLocked(), &a.expiresAt)
	return a.server, err
}

// Descriptor derives a peer address from current signed network authority.
// Paperboat authority mode uses explicit peer admission, pinned QUIC identity
// and operation grants rather than Tailcat's optional address PSK.
func (a *Authority) Descriptor(peerID string) (mesh.Addr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return "", authorityClosedError{}
	}
	if !a.usableLocked() {
		if a.expired {
			return "", authorityExpiredError{}
		}
		return "", authorityUnavailableError{}
	}
	if a.current.Self.Role != "cli" {
		return "", authorityRoleError{}
	}
	for _, peer := range a.current.Peers {
		if peer.Identity.EndpointID != peerID {
			continue
		}
		live := false
		for _, scope := range peer.Scopes {
			live = live || scopeLive(scope, time.Now().Unix())
		}
		if !live {
			return "", authorityPeerAdmissionError{}
		}
		node, err := publicKey(peer.Identity.WireGuardPublicKey)
		disco, discoErr := derpquic.ParseDiscoKey(peer.Identity.DiscoPublicKey)
		if err != nil || discoErr != nil || disco.IsZero() {
			return "", authorityPeerBindingError{}
		}
		a.relay.mu.Lock()
		regions := make([]*tailcfg.DERPRegion, 0, len(a.relay.nodes))
		for _, candidate := range a.relay.nodes {
			regions = append(regions, regionForNode(candidate))
		}
		a.relay.mu.Unlock()
		sort.Slice(regions, func(i, j int) bool { return regions[i].RegionID < regions[j].RegionID })
		if len(regions) > 1 {
			regions = regions[:1]
		}
		return (&mesh.ConnInfo{ServerPublic: mesh.NodePublic{NodePublic: node}, ServerDiscoPublic: mesh.DiscoPublic{DiscoPublic: disco}, Region: regions}).Addr(), nil
	}
	return "", authorityPeerAdmissionError{}
}

// Client binds a connection descriptor to the exact signed peer key before any
// network work. All peer leases share one authority-owned mesh engine and
// one aggregate flow bound; revoking a peer closes only that peer's leases.
func (a *Authority) Client(descriptor mesh.Addr, peerID string) (*UDPClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || !a.usableLocked() || a.current.Self.Role != "cli" {
		return nil, ErrAuthority
	}
	info, err := mesh.ParseAddr(descriptor)
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
		// Bootstrap hints may change without changing signed peer authority.
		// Keep live flows until identity, scopes or expiry actually withdraw them.
		if lease, ok := a.clients[peerID]; ok {
			lease.client.mu.Lock()
			closed := lease.client.closed
			lease.client.mu.Unlock()
			if !closed && lease.peer == p.Identity && (a.streamScopes || scopesRetained(lease.scopes, p.Scopes)) {
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
			controlPeers := append([]key.NodePublic(nil), a.relay.controlPeers...)
			a.relay.mu.Unlock()
			sort.Slice(regions, func(i, j int) bool { return regions[i].RegionID < regions[j].RegionID })
			var firstRegion *tailcfg.DERPRegion
			if len(regions) != 0 {
				firstRegion = regions[0]
			}
			a.clientEngine = &mesh.Server{OnRelayControl: a.relayControl, DERPCarrierFactory: factory, PeerRelayNodes: peerNodes, RelayControlPeers: controlPeers, TestOnlyPacketListener: a.options.TestOnlyPacketListener, Key: a.private, LocalAddr: netip.MustParseAddr(a.current.Self.VirtualAddress), AllowedPeers: peers, PeerContacts: a.peerContactsLocked(), OnLocalEndpoints: a.endpointsChanged, Region: firstRegion, Logf: func(string, ...any) {}}
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
		client := &UDPClient{expiresAt: &a.expiresAt, admit: func() bool { _, err := a.Peer(peerID); return err == nil }, dial: func(ctx context.Context) (mesh.ConnPacketConn, error) {
			return a.clientEngine.DialAuthorizedUDP(ctx, pub, disco, peerAddr)
		}, slots: a.clientSlots, port: NetworkPort, flows: make(map[*Packet]struct{}), ctx: ctx, cancel: cancel}
		a.clients[peerID] = authorizedClient{client: client, peer: p.Identity, scopes: append([]NetworkScope(nil), p.Scopes...), node: pub}
		return client, nil
	}
	return nil, ErrAdmission
}

func scopesRetained(old, current []NetworkScope) bool {
	for _, prior := range old {
		found := false
		for _, next := range current {
			if prior.ResourceKind == next.ResourceKind &&
				prior.ResourceID == next.ResourceID &&
				prior.ResourceGeneration == next.ResourceGeneration &&
				prior.Capability == next.Capability &&
				prior.Direction == next.Direction &&
				prior.Port == next.Port {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// RetireIdleClient forgets a failed cached route only when no dial or admitted
// packet flow still owns it. A failed extra handshake cannot evict live traffic.
func (a *Authority) RetireIdleClient(client *UDPClient) bool {
	if client == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, lease := range a.clients {
		if lease.client != client {
			continue
		}
		client.mu.Lock()
		if client.closed || client.count != 0 {
			client.mu.Unlock()
			return false
		}
		client.closed = true
		client.mu.Unlock()
		_ = client.Close()
		if a.clientEngine != nil {
			a.clientEngine.InvalidateAuthorizedPeer(lease.node)
		}
		delete(a.clients, id)
		return true
	}
	return false
}

// Static typed reasons preserve authority classification without private values.
type authorityClosedError struct{}

func (authorityClosedError) Error() string { return "peer network authority is closed" }
func (authorityClosedError) Unwrap() error { return ErrAuthority }

type authorityExpiredError struct{}

func (authorityExpiredError) Error() string   { return "peer network authority expired" }
func (authorityExpiredError) Unwrap() []error { return []error{ErrAuthority, ErrExpiredAuthority} }

type authorityUnavailableError struct{}

func (authorityUnavailableError) Error() string { return "peer network authority is unavailable" }
func (authorityUnavailableError) Unwrap() error { return ErrAuthority }

type authorityRoleError struct{}

func (authorityRoleError) Error() string { return "peer network authority role is invalid" }
func (authorityRoleError) Unwrap() error { return ErrAuthority }

type authorityPeerBindingError struct{}

func (authorityPeerBindingError) Error() string { return "peer network authority binding is invalid" }
func (authorityPeerBindingError) Unwrap() error { return ErrAuthority }

type authorityPeerAdmissionError struct{}

func (authorityPeerAdmissionError) Error() string { return "peer network admission refused" }
func (authorityPeerAdmissionError) Unwrap() error { return ErrAdmission }

func peerAdmissionExpiry(peer NetworkPeer) int64 {
	var expiry int64
	for _, scope := range peer.Scopes {
		if scope.ExpiresAt == 0 {
			return math.MaxInt64
		}
		expiry = max(expiry, scope.ExpiresAt)
	}
	return expiry
}

// scopeLive reports whether scope admits work at now. A machine_network scope with expires_at 0 lasts until
// revoked (F21-01): only a newer verified configuration without it ends it, never the clock.
func scopeLive(scope NetworkScope, now int64) bool {
	return scope.ExpiresAt == 0 || scope.ExpiresAt > now
}
func (a *Authority) peerAdmissionExpiriesLocked() map[netip.Addr]int64 {
	result := make(map[netip.Addr]int64, len(a.current.Peers))
	for _, peer := range a.current.Peers {
		result[netip.MustParseAddr(peer.Identity.VirtualAddress)] = peerAdmissionExpiry(peer)
	}
	return result
}
