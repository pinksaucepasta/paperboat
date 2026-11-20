package derpquic

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
	"github.com/quic-go/quic-go"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=keyed-peer-connection-registry
	"tailscale.com/types/key"
)

// The relay mesh: one QUIC connection per pair of Paperboat relays (the lower node id dials) on the relays' own UDP
// port, ALPN nodemesh.RelayMeshALPN, authenticated by the SPKI pins the control plane lists in the lease. A control
// stream carries which route keys each relay holds (Present / Gone / Snapshot) and the grants it is asked for
// (NeedGrant / Grant); forwarded packets are QUIC datagrams. Prior art: tailscale's DERP mesh
// (derp/derpserver/derpserver.go clientsMesh, AddPacketForwarder, addWatcher; derp/derphttp/mesh_client.go): local
// delivery always wins, a packet that arrived from the mesh is delivered locally only (hop limit 1), and a broken
// link clears everything that peer held and refloods on reconnect.
const (
	meshDialTimeout  = 3 * time.Second
	meshStreamWait   = 5 * time.Second
	meshWriteWait    = 5 * time.Second
	meshControlDepth = 1024 // frames queued to one link before it is judged out of sync and re-established
	meshDataDepth    = queueDepth
	meshNeedEvery    = time.Second // one NeedGrant per route key per second
	meshNeedKeys     = 1024
	meshStableAfter  = 10 * time.Second // a link this old resets the reconnect backoff
	defaultRetryBase = time.Second
	defaultRetryMax  = 30 * time.Second
	grantsPerKeyMax  = 2 * MaxGrantParts // tokens kept per carrier: a full credential and one refresh of it
)

// Reasons a mesh packet or frame was not carried (the index of Mesh.drops).
const (
	dropNoGrant = iota
	dropNoDestination
	dropUnpaired
	dropQueueFull
	dropDeliveryFull
	dropHolderFull
	dropControlOverflow
	dropSend
	dropDraining
	dropReasons
)

// MeshConfig configures a Mesh. Cert is the relay's own serving certificate: the one whose SPKI pin the control plane
// publishes to the other relays, presented as server and client certificate on mesh links.
type MeshConfig struct {
	Cert tls.Certificate
	// RetryBase and RetryMax bound the reconnect backoff (shared/backoff, jittered); zero means 1 s and 30 s.
	RetryBase, RetryMax time.Duration
}

// Mesh is one relay's side of the relay mesh. A Server owns it (SetMesh).
type Mesh struct {
	s                   *Server
	self                string
	cert                tls.Certificate
	retryBase, retryMax time.Duration
	serverTLS           *tls.Config
	peerSet             atomic.Pointer[nodemesh.PeerSet]

	// mu guards what follows. Lock order: Server.mu, then Mesh.mu.
	mu      sync.RWMutex
	links   map[string]*meshLink // by peer node id
	holders map[key.NodePublic]*meshLink
	// holderLimit bounds the remote route keys held: each costs a 32-byte key and a pointer. A relay holds at most
	// MaxConnections carriers, so the whole mesh fits four times that; past it new Presents are dropped (counted)
	// and a reconnect refloods, instead of growing with whatever a peer announces.
	holderLimit int
	remote      *remoteCache
	needed      map[key.NodePublic]time.Time
	dialers     map[string]*meshDialer
	started     bool
	closed      bool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	forwarded, received, bytesIn, bytesOut, holderMiss, unreachable, loopRefused atomic.Uint64
	drops                                                                        [dropReasons]atomic.Uint64
}

type meshDialer struct {
	peer   nodemesh.Peer
	cancel context.CancelFunc
}

// meshOut is one packet queued to a link.
type meshOut struct {
	src, dst key.NodePublic
	data     []byte
	control  bool
}

type meshLink struct {
	m        *Mesh
	peer     nodemesh.Peer
	conn     *quic.Conn
	stream   *quic.Stream
	ctrl     chan meshFrame
	out      chan meshOut
	sequence atomic.Uint64
	once     sync.Once
}

// NewMesh checks cfg; SetMesh attaches it to a server before Serve.
func NewMesh(cfg MeshConfig) (*Mesh, error) {
	if len(cfg.Cert.Certificate) == 0 || cfg.RetryBase < 0 || cfg.RetryMax < 0 {
		return nil, ErrProtocol
	}
	m := &Mesh{cert: cfg.Cert, retryBase: cfg.RetryBase, retryMax: cfg.RetryMax, links: make(map[string]*meshLink), holders: make(map[key.NodePublic]*meshLink),
		holderLimit: 4 * MaxConnections, remote: newRemoteCache(MaxConnections), needed: make(map[key.NodePublic]time.Time), dialers: make(map[string]*meshDialer)}
	if m.retryBase == 0 {
		m.retryBase = defaultRetryBase
	}
	if m.retryMax == 0 {
		m.retryMax = defaultRetryMax
	}
	empty, _ := nodemesh.NewPeerSet(nil)
	m.peerSet.Store(&empty)
	m.serverTLS = nodemesh.ServerTLS(cfg.Cert, nodemesh.RelayMeshALPN, func() nodemesh.PeerSet { return *m.peerSet.Load() })
	return m, nil
}

// SetMesh attaches the mesh; it must precede Serve.
func (s *Server) SetMesh(m *Mesh) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m == nil || s.mesh != nil || s.listener != nil || m.s != nil {
		return ErrProtocol
	}
	m.s, m.self = s, s.verifier.NodeID
	s.mesh = m
	return nil
}

// SetMeshPeers replaces the relays this one links to (the lease's mesh_peers; none means no mesh). A directory that
// fails validation is refused whole and the last good one stays. A server without a mesh ignores it.
func (s *Server) SetMeshPeers(peers []nodemesh.Peer) error {
	if s.mesh == nil {
		return nil
	}
	return s.mesh.SetPeers(peers)
}

// MeshStats are the mesh counters for the observation body; false when this relay has no mesh.
func (s *Server) MeshStats() (nodemesh.Stats, bool) {
	if s.mesh == nil {
		return nodemesh.Stats{}, false
	}
	return s.mesh.stats(), true
}

func (m *Mesh) stats() nodemesh.Stats {
	m.mu.RLock()
	links, names := len(m.links), len(m.holders)
	m.mu.RUnlock()
	return nodemesh.Stats{Links: links, Names: names, Forwarded: m.forwarded.Load(), Accepted: m.received.Load(), HolderMiss: m.holderMiss.Load(),
		Unreachable: m.unreachable.Load(), LoopRefused: m.loopRefused.Load(), BytesIn: m.bytesIn.Load(), BytesOut: m.bytesOut.Load()}
}

func (m *Mesh) drop(reason int) { m.drops[reason].Add(1) }

// SetPeers replaces the peer directory and starts or stops links to match.
func (m *Mesh) SetPeers(peers []nodemesh.Peer) error {
	others := slices.DeleteFunc(slices.Clone(peers), func(p nodemesh.Peer) bool { return p.NodeID == m.self })
	set, err := nodemesh.NewPeerSet(others)
	if err != nil {
		return err
	}
	m.peerSet.Store(&set)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconcileLocked(set)
	return nil
}

func samePeer(a, b nodemesh.Peer) bool {
	return a.Host == b.Host && a.Port == b.Port && strings.EqualFold(a.SPKISHA256, b.SPKISHA256) && a.ProcessEpoch == b.ProcessEpoch
}

// reconcileLocked starts a dialer for every peer with a higher node id than this relay (the lower id dials), stops
// those whose peer left or changed, and closes links (dialed or accepted) to peers that left or changed.
func (m *Mesh) reconcileLocked(set nodemesh.PeerSet) {
	if !m.started || m.closed {
		return
	}
	for id, d := range m.dialers {
		if p, ok := set.Lookup(id); !ok || !samePeer(p, d.peer) {
			d.cancel()
			delete(m.dialers, id)
		}
	}
	for id, l := range m.links {
		if p, ok := set.Lookup(id); !ok || !samePeer(p, l.peer) {
			l.close()
		}
	}
	for _, p := range set.Peers() {
		if m.self < p.NodeID && m.dialers[p.NodeID] == nil {
			ctx, cancel := context.WithCancel(m.ctx)
			m.dialers[p.NodeID] = &meshDialer{peer: p, cancel: cancel}
			m.wg.Add(1)
			go func() { defer m.wg.Done(); m.dialLoop(ctx, p) }()
		}
	}
}

// start runs once the QUIC transport listens: it begins dialing the peers already known.
func (m *Mesh) start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.started = true
	m.reconcileLocked(*m.peerSet.Load())
}

// close ends every dialer and link; Server.Wait waits for them.
func (m *Mesh) close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	links := make([]*meshLink, 0, len(m.links))
	for _, l := range m.links {
		links = append(links, l)
	}
	m.mu.Unlock()
	for _, l := range links {
		l.close()
	}
}

func (m *Mesh) wait() { m.wg.Wait() }

// dialLoop keeps one link to peer, redialling with jittered backoff (shared/backoff) so relays restarted together
// do not redial in step.
func (m *Mesh) dialLoop(ctx context.Context, peer nodemesh.Peer) {
	for attempt := 1; ctx.Err() == nil; {
		began := time.Now()
		err := m.dialOnce(ctx, peer)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) >= meshStableAfter {
			attempt = 1
		}
		if err != nil {
			slog.Warn("relay mesh link failed", "peer", peer.NodeID, "error", err)
		}
		timer := time.NewTimer(backoff.Retry(attempt, m.retryBase, m.retryMax))
		attempt++
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func resolveUDP(ctx context.Context, host string, port uint16) (*net.UDPAddr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, port)), nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err == nil && len(ips) == 0 {
		err = errors.New("no address")
	}
	if err != nil {
		return nil, fmt.Errorf("resolve mesh peer: %w", err)
	}
	return net.UDPAddrFromAddrPort(netip.AddrPortFrom(ips[0], port)), nil
}

// dialOnce dials peer on this relay's own listening socket and serves the link until it ends.
func (m *Mesh) dialOnce(ctx context.Context, peer nodemesh.Peer) error {
	config, err := nodemesh.ClientTLS(m.cert, nodemesh.RelayMeshALPN, peer)
	if err != nil {
		return err
	}
	dial, cancel := context.WithTimeout(ctx, meshDialTimeout)
	defer cancel()
	addr, err := resolveUDP(dial, peer.Host, peer.Port)
	if err != nil {
		return err
	}
	m.s.mu.Lock()
	transport := m.s.transport
	m.s.mu.Unlock()
	if transport == nil {
		return ErrClosed
	}
	conn, err := transport.Dial(dial, addr, config, quicConfig())
	if err != nil {
		return fmt.Errorf("dial mesh peer: %w", err)
	}
	stream, err := conn.OpenStreamSync(dial)
	if err != nil {
		conn.CloseWithError(0, "mesh stream")
		return fmt.Errorf("open mesh stream: %w", err)
	}
	m.run(m.newLink(peer, conn, stream))
	return nil
}

// accept serves a link a higher-id relay dialled; the handshake already proved its pin is in the directory.
func (m *Mesh) accept(c *quic.Conn) {
	defer c.CloseWithError(0, "mesh closed")
	peer, ok := nodemesh.PeerOf(c.ConnectionState().TLS, *m.peerSet.Load())
	if !ok || peer.NodeID >= m.self {
		return // not a listed relay, or the wrong end dialled (the lower id dials)
	}
	ctx, cancel := context.WithTimeout(c.Context(), meshStreamWait)
	stream, err := c.AcceptStream(ctx)
	cancel()
	if err != nil {
		return
	}
	m.run(m.newLink(peer, c, stream))
}

func (m *Mesh) newLink(peer nodemesh.Peer, c *quic.Conn, stream *quic.Stream) *meshLink {
	return &meshLink{m: m, peer: peer, conn: c, stream: stream, ctrl: make(chan meshFrame, meshControlDepth), out: make(chan meshOut, meshDataDepth)}
}

func (l *meshLink) close() { l.once.Do(func() { l.conn.CloseWithError(0, "mesh link closed") }) }

// send queues a control frame; a link that cannot take it has lost sync with this relay, so it is closed and
// re-established (the reconnect refloods) instead of dropping a Present or Gone silently.
func (l *meshLink) send(f meshFrame) {
	select {
	case l.ctrl <- f:
	default:
		l.m.drop(dropControlOverflow)
		l.close()
	}
}

// run serves one link: register (snapshot first), then the control reader here and the writers and the datagram
// reader beside it, until the connection ends; then withdraw everything the peer held.
func (m *Mesh) run(l *meshLink) {
	defer l.close()
	if !m.register(l) {
		return
	}
	defer m.unregister(l)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); l.writeControl() }()
	go func() { defer wg.Done(); l.writeData() }()
	go func() { defer wg.Done(); l.readData() }()
	l.readControl()
	l.close()
	wg.Wait()
}

// register queues the snapshot of this relay's carriers and then lists the link, so a carrier that connects after
// it is announced by a live Present behind the snapshot, never between (DERP addWatcher floods under the lock).
func (m *Mesh) register(l *meshLink) bool {
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	keys := make([]string, 0, min(len(s.peers), meshKeysPerFrame))
	for k := range s.peers {
		keys = append(keys, KeyString(k))
		if len(keys) == meshKeysPerFrame {
			l.ctrl <- meshFrame{Kind: meshSnapshot, Keys: keys, More: true}
			keys = make([]string, 0, meshKeysPerFrame)
		}
	}
	l.ctrl <- meshFrame{Kind: meshSnapshot, Keys: keys}
	if old := m.links[l.peer.NodeID]; old != nil {
		old.close() // a reconnect that beat the old link's timeout replaces it
	}
	m.links[l.peer.NodeID] = l
	return true
}

// unregister is DERP's clear-on-break: what the peer held is gone with the link, and its reconnect refloods.
func (m *Mesh) unregister(l *meshLink) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.links[l.peer.NodeID] == l {
		delete(m.links, l.peer.NodeID)
	}
	for k, h := range m.holders {
		if h == l {
			delete(m.holders, k)
		}
	}
	m.remote.removeVia(l)
}

func (l *meshLink) writeControl() {
	ctx := l.conn.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-l.ctrl:
			l.stream.SetWriteDeadline(time.Now().Add(meshWriteWait))
			if nodemesh.WriteMessage(l.stream, f) != nil {
				l.close()
				return
			}
		}
	}
}

// writeData sends the queued packets as datagrams. The hop is counted where it succeeds, once.
func (l *meshLink) writeData() {
	ctx := l.conn.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-l.out:
			if err := sendFragments(l.conn, &l.sequence, o.src, &o.dst, o.data, o.control); err != nil {
				if ctx.Err() != nil {
					return
				}
				l.m.drop(dropSend)
				continue
			}
			l.m.forwarded.Add(1)
			l.m.bytesOut.Add(uint64(len(o.data)))
		}
	}
}

func (l *meshLink) readData() {
	defer l.close()
	r := reassembler{mesh: true}
	for {
		b, err := l.conn.ReceiveDatagram(l.conn.Context())
		if err != nil {
			return
		}
		p, ok, err := r.acceptFragment(b, time.Now(), nil)
		if err != nil {
			return
		}
		if ok {
			l.m.deliver(l, p)
		}
	}
}

func (l *meshLink) readControl() {
	for {
		var f meshFrame
		if err := nodemesh.ReadMessage(l.stream, &f); err != nil || f.Validate() != nil || l.m.handle(l, f) != nil {
			return
		}
	}
}

// handle applies one control frame from a link; an error ends the link.
func (m *Mesh) handle(l *meshLink, f meshFrame) error {
	switch f.Kind {
	case meshPresent, meshSnapshot:
		keys, err := f.routeKeys()
		if err != nil {
			return err
		}
		m.mu.Lock()
		for _, k := range keys {
			if _, held := m.holders[k]; !held && len(m.holders) >= m.holderLimit {
				m.drop(dropHolderFull)
				continue
			}
			m.holders[k] = l
		}
		m.mu.Unlock()
	case meshGone:
		keys, err := f.routeKeys()
		if err != nil {
			return err
		}
		m.mu.Lock()
		for _, k := range keys {
			if m.holders[k] == l {
				delete(m.holders, k)
			}
			if e := m.remote.index[k]; e != nil && e.Value.(*remoteSource).via == l {
				m.remote.remove(k)
			}
		}
		m.mu.Unlock()
	case meshNeedGrant:
		return m.answerNeed(l, f)
	case meshGrant:
		return m.acceptGrant(l, f)
	}
	return nil
}

// answerNeed sends the signed grants of the carrier connected here under the asked route key.
func (m *Mesh) answerNeed(l *meshLink, f meshFrame) error {
	route, err := ParseKey(f.Key)
	if err != nil {
		return err
	}
	s := m.s
	s.mu.Lock()
	var tokens []string
	var namespace [32]byte
	if p := s.peers[route]; p != nil {
		tokens, namespace = slices.Clone(p.tokens), p.namespace
	}
	s.mu.Unlock()
	for _, token := range tokens {
		l.send(meshFrame{Kind: meshGrant, Key: f.Key, Namespace: namespaceHex(namespace), Token: token})
	}
	return nil
}

// acceptGrant verifies one grant token a peer relays and caches it, merging parts of one grant like admit does.
// A token that does not verify is not a protocol error: it may be expired, and the packet it was for is gone.
func (m *Mesh) acceptGrant(l *meshLink, f meshFrame) error {
	route, err := ParseKey(f.Key)
	namespace, ok := parseNamespace(f.Namespace)
	if err != nil || !ok {
		return errMeshFrame
	}
	s, now := m.s, time.Now()
	g, err := s.verifier.verifyRemoteGrant(f.Token, *s.keys.Load(), l.peer.NodeID, route, namespace, now)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.remote.get(route, now); old != nil && old.via == l {
		switch {
		case sameGrantIdentity(old.grant, g):
			merged, err := mergeGrants(old.grant, g)
			if err != nil {
				return nil
			}
			g = merged
		case g.Generation < old.grant.Generation:
			return nil
		}
	}
	m.remote.put(newRemoteSource(g, route, namespace, l))
	return nil
}

// needGrant asks the relay that forwarded a packet for its sender's grants, at most once a second per sender.
func (m *Mesh) needGrant(l *meshLink, route key.NodePublic) {
	now := time.Now()
	m.mu.Lock()
	if last, ok := m.needed[route]; ok && now.Sub(last) < meshNeedEvery {
		m.mu.Unlock()
		return
	}
	if len(m.needed) >= meshNeedKeys {
		for k, at := range m.needed {
			if now.Sub(at) >= meshNeedEvery {
				delete(m.needed, k)
			}
		}
		if len(m.needed) >= meshNeedKeys {
			m.mu.Unlock()
			return
		}
	}
	m.needed[route] = now
	m.mu.Unlock()
	l.send(meshFrame{Kind: meshNeedGrant, Key: KeyString(route)})
}

// deliver hands a packet that arrived over a link to the local destination. It never forwards: a packet from the
// mesh is delivered here or dropped (hop limit 1). Authorization is this relay's own: the sender's verified grant
// against the destination's. The ingress relay already charged the carrier and the account, so nothing is charged
// here; the delivery counts the forwarded bytes once, in writePackets.
func (m *Mesh) deliver(l *meshLink, p meshPacket) {
	now := time.Now()
	m.bytesIn.Add(uint64(len(p.data)))
	m.mu.Lock()
	rs := m.remote.get(p.peer, now)
	m.mu.Unlock()
	if rs == nil || rs.via != l {
		m.drop(dropNoGrant)
		m.needGrant(l, p.peer)
		return
	}
	s := m.s
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.peers[p.dst]
	switch {
	case q == nil:
		m.loopRefused.Add(1)
		m.drop(dropNoDestination)
	case s.draining && !now.Before(s.deadline):
		m.drop(dropDraining)
	case !s.authorizedRemoteLocked(rs, q, p.control, now):
		m.drop(dropUnpaired)
	default:
		select {
		case q.out <- queued{packet: packet{peer: rs.root, data: p.data, control: p.control}, remote: rs}:
			m.received.Add(1)
		default:
			m.drop(dropDeliveryFull)
		}
	}
}

// forwardLocked is Server.route's miss path: the destination is not connected here, so a relay that holds it gets
// the packet. A local carrier always wins (route only misses when there is none).
func (s *Server) forwardLocked(p *peerConn, dst key.NodePublic, msg packet) bool {
	m := s.mesh
	if m == nil || s.peers[routeKey(p.key, p.namespace)] != p || s.draining && !time.Now().Before(s.deadline) {
		return false
	}
	m.mu.RLock()
	l := m.holders[dst]
	m.mu.RUnlock()
	if l == nil {
		m.holderMiss.Add(1)
		return false
	}
	select {
	case l.out <- meshOut{src: routeKey(p.key, p.namespace), dst: dst, data: msg.data, control: msg.control}:
	default:
		m.unreachable.Add(1)
		m.drop(dropQueueFull)
	}
	return true
}

// holds reports whether a relay of the mesh holds the route key. Nil-safe: no mesh holds nothing.
func (m *Mesh) holds(k key.NodePublic) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.holders[k] != nil
}

// announce tells every link a route key appeared or left this relay. Called with Server.mu held.
func (m *Mesh) announce(kind string, k key.NodePublic) {
	if m == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, l := range m.links {
		l.send(meshFrame{Kind: kind, Keys: []string{KeyString(k)}})
	}
}

// rememberTokens keeps the signed grant tokens a carrier authenticated with, to answer a NeedGrant. A further part of
// the same generation adds to them (as admit merges it), a new generation replaces them; the list stays bounded.
func (s *Server) rememberTokens(p *peerConn, g Grant, tokens []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.tokenGeneration == g.Generation {
		for _, t := range tokens {
			if !slices.Contains(p.tokens, t) {
				p.tokens = append(p.tokens, t)
			}
		}
		if extra := len(p.tokens) - grantsPerKeyMax; extra > 0 {
			p.tokens = p.tokens[extra:]
		}
	} else {
		p.tokens, p.tokenGeneration = slices.Clone(tokens), g.Generation
	}
}
