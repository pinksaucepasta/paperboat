package forward

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// Reportcause stages of a failed forward or background mesh call.
const (
	StageLookup = "forward_lookup"
	StageDial   = "forward_dial"
	StageOpen   = "forward_open"
)

// Outcomes counted by paperboat_edge_forward_total.
const (
	OutcomeForwarded   = "forwarded"
	OutcomeHolderMiss  = "holder_miss"
	OutcomeUnreachable = "unreachable"
	OutcomeLoopRefused = "loop_refused"
	OutcomeRefused     = "refused"
)

const (
	maxPendingHandshakes = 64
	heldInterval         = 250 * time.Millisecond
	writeDeadline        = 10 * time.Second
	minLinkLife          = 5 * time.Second
)

// PeerSource answers the server's peer directory and the caller's own region.
type PeerSource interface {
	Peers(ctx context.Context) (nodemesh.PeersResponse, error)
}

type HoldersSource interface {
	Holders(ctx context.Context, page string, limit int) (nodemesh.HoldersResponse, error)
}

// HeldSource lists what this node serves now: hostnames (and raw TCP keys) whose carrier is attached.
type HeldSource interface{ Held() []string }

// Injector takes a visitor forwarded by a peer into this node's HTTPS listener. It owns conn once it returns nil.
type Injector interface {
	Inject(conn net.Conn, visitor net.Addr) error
}

// TCPInjector takes a visitor forwarded by a peer for a raw TCP tunnel's public port.
type TCPInjector interface {
	InjectTCP(conn net.Conn, port uint16) error
}

// Config wires a Mesh. Sources, Held and Injector are required; zero timeouts and limits are the config defaults.
type Config struct {
	NodeID string
	// Certificate is the node's registered carrier certificate: the one whose pin the server published.
	Certificate tls.Certificate
	Peers       PeerSource
	Holders     HoldersSource
	Held        HeldSource
	Injector    Injector
	TCPInjector TCPInjector

	DialTimeout, OpenTimeout, PeerInterval, SeedInterval time.Duration
	// MaxPeerStale is how long the last good peer directory stays trusted while refreshes fail (default 5x PeerInterval);
	// past it the set is emptied, so links close and a retired or rekeyed peer cannot keep forwarding through a
	// control-plane outage. The next good refresh restores it.
	MaxPeerStale time.Duration
	// HeldInterval is how often the node's holdings are diffed into the watch streams (default 250 ms); tests lengthen it
	// to keep the peers' maps stale on purpose.
	HeldInterval      time.Duration
	MaxStreamsPerPeer int

	// Dial opens the TCP connection to a peer (default net.Dialer); tests replace it.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// OnFailure reports a background failure once per run of consecutive failures; OnOutcome counts one forward.
	OnFailure func(stage string, err error)
	OnOutcome func(outcome string)
	Now       func() time.Time
}

// Mesh is one node's side of the node mesh: the peer links, the holder directory and the forwarder.
type Mesh struct {
	cfg   Config
	dir   *Directory
	peers atomic.Pointer[nodemesh.PeerSet]

	handshakes chan struct{}
	peersReady chan struct{}
	readyOnce  sync.Once

	mu    sync.Mutex
	ctx   context.Context
	stop  context.CancelFunc
	wg    sync.WaitGroup
	links map[string]*link
	loops map[string]*dialLoop
	// pending are the handshakes in flight; applyPeers cuts those of a peer that left or changed.
	pending   map[*pendingHandshake]struct{}
	peersOKAt time.Time // last successful peer refresh

	held   atomic.Pointer[heldSnapshot]
	heldMu sync.Mutex
	heldAt time.Time

	seedMu   sync.Mutex
	seededAt time.Time

	reportMu sync.Mutex
	reported map[string]bool

	forwarded, accepted, holderMiss, unreachable, loopRefused, bytesIn, bytesOut atomic.Uint64
}

type dialLoop struct {
	peer   nodemesh.Peer
	cancel context.CancelFunc
}

// New builds a stopped Mesh.
func New(cfg Config) (*Mesh, error) {
	if cfg.NodeID == "" || cfg.Peers == nil || cfg.Holders == nil || cfg.Held == nil || cfg.Injector == nil || len(cfg.Certificate.Certificate) == 0 {
		return nil, errors.New("forward mesh configuration is invalid")
	}
	for _, d := range []*time.Duration{&cfg.DialTimeout, &cfg.OpenTimeout} {
		if *d <= 0 {
			*d = 3 * time.Second
		}
	}
	if cfg.PeerInterval <= 0 {
		cfg.PeerInterval = 10 * time.Second
	}
	if cfg.MaxPeerStale <= 0 {
		cfg.MaxPeerStale = 5 * cfg.PeerInterval
	}
	if cfg.SeedInterval <= 0 {
		cfg.SeedInterval = 5 * time.Minute
	}
	if cfg.HeldInterval <= 0 {
		cfg.HeldInterval = heldInterval
	}
	if cfg.MaxStreamsPerPeer <= 0 {
		cfg.MaxStreamsPerPeer = 4096
	}
	if cfg.Dial == nil {
		cfg.Dial = (&net.Dialer{}).DialContext
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	m := &Mesh{cfg: cfg, dir: NewDirectory(2*cfg.SeedInterval, cfg.Now), handshakes: make(chan struct{}, maxPendingHandshakes), peersReady: make(chan struct{}), links: map[string]*link{}, pending: map[*pendingHandshake]struct{}{}, loops: map[string]*dialLoop{}, reported: map[string]bool{}}
	empty, _ := nodemesh.NewPeerSet(nil)
	m.peers.Store(&empty)
	return m, nil
}

// Directory is the holder map, for tests and diagnostics.
func (m *Mesh) Directory() *Directory { return m.dir }

// Start runs the mesh until ctx ends or Stop. It returns at once.
func (m *Mesh) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil {
		return errors.New("forward mesh already started")
	}
	ctx, m.stop = context.WithCancel(ctx)
	m.ctx = ctx
	m.refreshHeld()
	m.wg.Go(func() { m.peerLoop(ctx) })
	m.wg.Go(func() { m.heldLoop(ctx) })
	m.wg.Go(func() { m.seedLoop(ctx) })
	m.wg.Go(func() {
		<-ctx.Done()
		m.closeLinks()
	})
	return nil
}

// Stop ends the loops and every link, and waits for them.
func (m *Mesh) Stop(ctx context.Context) error {
	m.mu.Lock()
	stop := m.stop
	m.mu.Unlock()
	if stop == nil {
		return nil
	}
	stop()
	m.closeLinks()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Mesh) closeLinks() {
	m.mu.Lock()
	for _, loop := range m.loops {
		loop.cancel()
	}
	links := make([]*link, 0, len(m.links))
	for _, l := range m.links {
		links = append(links, l)
	}
	m.mu.Unlock()
	for _, l := range links {
		_ = l.sess.Close()
	}
}

// Stats are the counters the heartbeat reports.
func (m *Mesh) Stats() nodemesh.Stats {
	m.mu.Lock()
	links := 0
	for _, l := range m.links {
		if !l.sess.IsClosed() {
			links++
		}
	}
	m.mu.Unlock()
	return nodemesh.Stats{Links: links, Names: m.dir.Names(), Forwarded: m.forwarded.Load(), Accepted: m.accepted.Load(), HolderMiss: m.holderMiss.Load(), Unreachable: m.unreachable.Load(), LoopRefused: m.loopRefused.Load(), BytesIn: m.bytesIn.Load(), BytesOut: m.bytesOut.Load()}
}

// report passes a background failure on once per run of consecutive failures of the same call.
func (m *Mesh) report(stage, call string, err error) {
	m.reportMu.Lock()
	first := !m.reported[call]
	m.reported[call] = err != nil
	m.reportMu.Unlock()
	if err != nil && first && m.cfg.OnFailure != nil {
		m.cfg.OnFailure(stage, fmt.Errorf("%s: %w", call, err))
	}
}

func (m *Mesh) outcome(name string) {
	if m.cfg.OnOutcome != nil {
		m.cfg.OnOutcome(name)
	}
}

func (m *Mesh) peerSet() nodemesh.PeerSet { return *m.peers.Load() }

// PeerCurrent reports whether nodeID is in the current peer set (and, when a link to it is up, still under the pin
// the link was made with). It is false once the set expired or the peer left it.
func (m *Mesh) PeerCurrent(nodeID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.peerSet().Lookup(nodeID)
	if l := m.links[nodeID]; ok && l != nil {
		return peerMatches(cur, l.peer)
	}
	return ok
}

// peerMatches reports whether a link made with peer is still valid against the current directory entry cur: the
// same pin and address, and the same process epoch when the directory names one.
func peerMatches(cur, peer nodemesh.Peer) bool {
	return cur.SPKISHA256 == peer.SPKISHA256 && cur.Host == peer.Host && cur.Port == peer.Port &&
		(cur.ProcessEpoch == "" || cur.ProcessEpoch == peer.ProcessEpoch)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// peerLoop refreshes the peer directory from the server and keeps one link loop per peer this node dials.
func (m *Mesh) peerLoop(ctx context.Context) {
	for {
		m.refreshPeers(ctx)
		if !sleep(ctx, backoff.Jitter(m.cfg.PeerInterval)) {
			return
		}
	}
}

func (m *Mesh) refreshPeers(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 2*m.cfg.DialTimeout+m.cfg.OpenTimeout)
	defer cancel()
	answer, err := m.cfg.Peers.Peers(rctx)
	if err == nil {
		kept := answer.Peers[:0:0]
		for _, p := range answer.Peers {
			if p.NodeID != m.cfg.NodeID {
				kept = append(kept, p)
			}
		}
		var set nodemesh.PeerSet
		if set, err = nodemesh.NewPeerSet(kept); err == nil {
			m.report(StageLookup, "peer directory", nil)
			if answer.Region != "" { // an answer without a region keeps the last one
				m.dir.SetRegion(answer.Region)
			}
			m.mu.Lock()
			m.peersOKAt = m.cfg.Now()
			m.mu.Unlock()
			m.applyPeers(ctx, set)
			m.readyOnce.Do(func() { close(m.peersReady) })
			return
		}
	}
	m.report(StageLookup, "peer directory", err)
	m.expirePeers(ctx)
}

// expirePeers empties the peer set once the last good refresh is older than MaxPeerStale: the applyPeers path then
// closes the links and loops and the directory withdraws the peers' names. A later good refresh restores them.
func (m *Mesh) expirePeers(ctx context.Context) {
	m.mu.Lock()
	stale := m.cfg.Now().Sub(m.peersOKAt) > m.cfg.MaxPeerStale
	m.mu.Unlock()
	if stale && m.peerSet().Len() > 0 {
		empty, _ := nodemesh.NewPeerSet(nil)
		m.applyPeers(ctx, empty)
	}
}

func (m *Mesh) applyPeers(ctx context.Context, set nodemesh.PeerSet) {
	m.peers.Store(&set)
	m.dir.SetPeers(set.Peers())
	var stale []*link
	var cut []context.CancelFunc
	m.mu.Lock()
	for id, loop := range m.loops {
		if p, ok := set.Lookup(id); !ok || p != loop.peer {
			loop.cancel()
			delete(m.loops, id)
		}
	}
	for id, l := range m.links {
		if p, ok := set.Lookup(id); !ok || !peerMatches(p, l.peer) {
			stale = append(stale, l)
		}
	}
	for h := range m.pending {
		if h.stale(set) {
			cut = append(cut, h.cancel)
		}
	}
	for _, p := range set.Peers() {
		if _, running := m.loops[p.NodeID]; running || m.cfg.NodeID > p.NodeID || m.ctx == nil {
			continue // the lower node id dials; the other waits for the link (or dials on demand)
		}
		lctx, cancel := context.WithCancel(ctx)
		m.loops[p.NodeID] = &dialLoop{peer: p, cancel: cancel}
		m.wg.Go(func() { m.dialLoop(lctx, p) })
	}
	m.mu.Unlock()
	for _, cancel := range cut {
		cancel()
	}
	for _, l := range stale {
		_ = l.sess.Close()
	}
}

// dialLoop keeps a link to p up, redialling with backoff and jitter. A link that dies soon after it came up
// (the peer refused it) counts as a failure, so a refused pin is not retried in a tight loop.
func (m *Mesh) dialLoop(ctx context.Context, p nodemesh.Peer) {
	failures := 0
	for ctx.Err() == nil {
		dctx, cancel := context.WithTimeout(ctx, 2*m.cfg.DialTimeout)
		l, err := m.connect(dctx, p)
		cancel()
		if err != nil {
			failures++
			m.report(StageDial, "link "+p.NodeID, err)
			if !sleep(ctx, backoff.Retry(failures, 500*time.Millisecond, 30*time.Second)) {
				return
			}
			continue
		}
		m.report(StageDial, "link "+p.NodeID, nil)
		select {
		case <-l.sess.CloseChan():
		case <-ctx.Done():
			return
		}
		if time.Since(l.started) < minLinkLife {
			failures++
		} else {
			failures = 0
		}
		if !sleep(ctx, backoff.Retry(max(failures, 1), 250*time.Millisecond, 30*time.Second)) {
			return
		}
	}
}

// yamuxConfig: the node link carries many visitors, so windows are modest and the incoming-stream cap is the
// per-peer cap plus the watch stream.
func (m *Mesh) yamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.LogOutput = io.Discard
	c.AcceptBacklog = 256
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 15 * time.Second
	c.ConnectionWriteTimeout = writeDeadline
	c.InitialStreamWindowSize = 256 << 10
	c.MaxStreamWindowSize = 1 << 20
	c.MaxIncomingStreams = uint32(m.cfg.MaxStreamsPerPeer + 8)
	return c
}
