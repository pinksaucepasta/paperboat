package tailnet

import (
	"context"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=delegated-generation-nat-traversal
	"tailscale.com/tailcfg"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=delegated-generation-nat-traversal
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=delegated-generation-nat-traversal
	"tailscale.com/wgengine/filter"
)

// CarrierNetwork isolates WireGuard/discovery state without creating a control
// plane endpoint. Authenticated old connections supply the generation's peer
// candidates. Magicsock supplies STUN, simultaneous disco probes, UDP peer relay
// and regional QUIC/WSS fallback (~/ref/tailscale/wgengine/magicsock/endpoint.go).
type CarrierNetwork struct {
	authority *Authority
	server    *mesh.Server
	disco     key.DiscoPublic
	self      NetworkBinding
	namespace [32]byte
	mu        sync.Mutex
	bindings  map[string]NetworkBinding
	contacts  map[key.NodePublic]mesh.PeerContact
	changedMu sync.Mutex
	changed   chan struct{} // closed and replaced on each endpoint change
	closed    bool
	sealed    atomic.Bool
	cancel    context.CancelFunc
}

func (a *Authority) NewCarrierNetwork(ctx context.Context, namespace [32]byte, incoming func(mesh.ConnPacketConn)) (*CarrierNetwork, error) {
	if ctx == nil || namespace == ([32]byte{}) {
		return nil, ErrAuthority
	}
	a.mu.Lock()
	if !a.usableLocked() {
		a.mu.Unlock()
		return nil, ErrAuthority
	}
	self, private := a.current.Self, a.private
	peers, _ := a.peersLocked()
	a.relay.mu.Lock()
	var regions []*tailcfg.DERPRegion
	for _, node := range a.relay.nodes {
		regions = append(regions, regionForNode(node))
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].RegionID < regions[j].RegionID })
	peerNodes, controlPeers := a.relay.peerNodes, a.relay.controlPeers
	relayTLS := a.relay.config
	a.relay.mu.Unlock()
	a.mu.Unlock()
	disco := mesh.DiscoPrivateForGeneration(private, namespace)
	c := &CarrierNetwork{authority: a, self: self, namespace: namespace, disco: disco.Public(), bindings: make(map[string]NetworkBinding), contacts: make(map[key.NodePublic]mesh.PeerContact), changed: make(chan struct{})}
	c.server = &mesh.Server{
		Key: private, DiscoPrivate: disco, LocalAddr: netip.MustParseAddr(self.VirtualAddress),
		AllowedPeers: peers, PeerContacts: make(map[key.NodePublic]mesh.PeerContact),
		PeerRelayNodes: peerNodes, RelayControlPeers: controlPeers,

		TestOnlyPacketListener: a.options.TestOnlyPacketListener,
		OnLocalEndpoints: func() {
			c.changedMu.Lock()
			close(c.changed)
			c.changed = make(chan struct{})
			c.changedMu.Unlock()
		},
	}
	if len(regions) != 0 {
		if relayTLS == nil {
			return nil, ErrRegionalAuthority
		}
		c.server.DERPCarrierFactory = a.regionalCarrierFactory(relayTLS, namespace, disco.Public())
	}
	if incoming != nil {
		c.server.ServedUDPPorts = []filter.PortRange{{First: NetworkPort, Last: NetworkPort}}
		c.server.OnUDP = func(port uint16) func(mesh.ConnPacketConn) {
			if port != NetworkPort || c.sealed.Load() {
				return nil
			}
			return incoming
		}
	}
	if err := c.server.SetRelayRegions(regions); err != nil {
		return nil, err
	}
	if err := c.server.Start(); err != nil {
		return nil, err
	}
	a.relay.mu.Lock()
	if a.relay.generationUsage == nil {
		a.relay.generationUsage = make(map[[32]byte]map[key.NodePublic]*relayUsageObservation)
	}
	observations := make(map[key.NodePublic]*relayUsageObservation)
	for peer := range peers {
		observations[peer] = &relayUsageObservation{}
	}
	a.relay.generationUsage[namespace] = observations
	a.relay.mu.Unlock()
	return c, nil
}

// AddPeer receives only candidates authenticated by the old native connection.
// It never substitutes discovery for the root's current signed peer authority.
func (c *CarrierNetwork) AddPeer(endpointID, discoPublic string, endpoints []string) error {
	binding, err := c.authority.Peer(endpointID)
	if err != nil {
		return err
	}
	public, err := publicKey(binding.WireGuardPublicKey)
	if err != nil {
		return err
	}
	disco, err := derpquic.ParseDiscoKey(discoPublic)
	if err != nil || disco.IsZero() || !validPeerEndpoints(endpoints) {
		return ErrAuthority
	}
	contact := mesh.PeerContact{Disco: disco}
	for _, raw := range endpoints {
		contact.Endpoints = append(contact.Endpoints, netip.MustParseAddrPort(raw))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if old, ok := c.contacts[public]; ok {
		contact.Live = old.Live
	}
	c.bindings[endpointID], c.contacts[public] = binding, contact
	return c.server.SetPeerContacts(c.contacts)
}

func (c *CarrierNetwork) DiscoPublic() string         { return derpquic.DiscoKeyString(c.disco) }
func (c *CarrierNetwork) Endpoints() []netip.AddrPort { return c.server.LocalEndpoints() }

// WaitEndpoints returns once the network knows a local endpoint. Every waiter
// wakes: two connections of one client moving at once both wait here (dadape
// direct1f: a one-slot signal woke one and left the other the whole budget).
func (c *CarrierNetwork) WaitEndpoints(ctx context.Context) error {
	for {
		c.changedMu.Lock()
		changed := c.changed
		c.changedMu.Unlock()
		if len(c.Endpoints()) != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (c *CarrierNetwork) Dial(ctx context.Context, endpointID string) (mesh.ConnPacketConn, error) {
	binding, err := c.authority.Peer(endpointID)
	if err != nil {
		return nil, err
	}
	public, err := publicKey(binding.WireGuardPublicKey)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	contact, ok := c.contacts[public]
	c.mu.Unlock()
	if !ok {
		return nil, ErrAuthority
	}
	conn, err := c.server.DialAuthorizedUDP(ctx, public, contact.Disco, netip.AddrPortFrom(netip.MustParseAddr(binding.VirtualAddress), NetworkPort))
	if err == nil {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			conn.Close()
			return nil, net.ErrClosed
		}
		contact = c.contacts[public]
		contact.Live = true
		c.contacts[public] = contact
		c.mu.Unlock()
	}
	return conn, err
}

func (c *CarrierNetwork) PeerPath(endpointID string) string {
	binding, err := c.authority.Peer(endpointID)
	if err != nil {
		return "unknown"
	}
	public, err := publicKey(binding.WireGuardPublicKey)
	if err != nil {
		return "unknown"
	}
	return c.server.PeerPath(public)
}

func (c *CarrierNetwork) UsagePath(endpointID, direction string) mesh.UsagePath {
	binding, err := c.authority.Peer(endpointID)
	if err != nil {
		return mesh.UsagePath{Mode: "unknown"}
	}
	public, err := publicKey(binding.WireGuardPublicKey)
	if err != nil {
		return mesh.UsagePath{Mode: "unknown"}
	}
	path := c.server.PeerUsagePath(public)
	if path.IsRegionalRelay() {
		path.NodeID = c.authority.relay.generationUsageNode(c.namespace, public, direction, time.Now())
	}
	return path
}

// Seal rejects new virtual QUIC flows after promotion. Existing authenticated
// connections keep their packet sockets until the bounded old-generation drain.
func (c *CarrierNetwork) Seal()        { c.sealed.Store(true) }
func (c *CarrierNetwork) Sealed() bool { return c.sealed.Load() }

func (c *CarrierNetwork) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.authority.relay.mu.Lock()
	delete(c.authority.relay.generationUsage, c.namespace)
	c.authority.relay.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	return c.server.Close()
}
