package forward

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// link is one authenticated yamux session to a peer. Either side opens streams: a watch stream each way, and one
// forward stream per forwarded visitor.
type link struct {
	peer    nodemesh.Peer
	sess    *yamux.Session
	remote  net.Addr
	dialer  string // node id that dialled the link
	started time.Time
	// notAfter is when the peer's certificate expires; the link ends then and the dial loop reconnects (zero: none).
	notAfter time.Time
	active   atomic.Int64 // inbound forwarded visitors
}

// errLinkRefused: install did not keep the link (the mesh stopped, or the peer left the directory or changed while
// the handshake ran).
var errLinkRefused = errors.New("link not installed: mesh stopped or peer no longer in the directory")

func (m *Mesh) liveLink(id string) *link {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.links[id]; l != nil && !l.sess.IsClosed() {
		return l
	}
	return nil
}

// connect returns the live link to p, dialling one if none is up.
func (m *Mesh) connect(ctx context.Context, p nodemesh.Peer) (*link, error) {
	if l := m.liveLink(p.NodeID); l != nil {
		return l, nil
	}
	return m.dial(ctx, p)
}

func (m *Mesh) dial(ctx context.Context, p nodemesh.Peer) (*link, error) {
	ctx, done := m.track(ctx, &pendingHandshake{peer: p, dial: true})
	defer done()
	conf, err := nodemesh.ClientTLS(m.cfg.Certificate, nodemesh.EdgeForwardALPN, p)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, m.cfg.DialTimeout)
	defer cancel()
	raw, err := m.cfg.Dial(dctx, "tcp", p.Address())
	if err != nil {
		return nil, fmt.Errorf("dial peer %s: %w", p.NodeID, err)
	}
	conn := tls.Client(raw, conf)
	if err := conn.HandshakeContext(dctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("handshake with peer %s: %w", p.NodeID, err)
	}
	if conn.ConnectionState().NegotiatedProtocol != nodemesh.EdgeForwardALPN {
		_ = raw.Close()
		return nil, fmt.Errorf("peer %s does not speak the node link", p.NodeID)
	}
	sess, err := yamux.Client(conn, m.yamuxConfig(), nil)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	// TLS 1.3 completes the client side before the server checked our certificate: a ping proves it accepted us.
	if err := ping(dctx, sess); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("peer %s refused the link: %w", p.NodeID, err)
	}
	l := &link{peer: p, sess: sess, remote: raw.RemoteAddr(), dialer: m.cfg.NodeID, started: time.Now(), notAfter: certEnd(conn)}
	won, installed := m.install(l)
	if !installed && won == l {
		return nil, errLinkRefused
	}
	return won, nil
}

func ping(ctx context.Context, sess *yamux.Session) error {
	done := make(chan error, 1)
	go func() { _, err := sess.Ping(); done <- err }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Accept takes an inbound node link: conn is a connection whose ClientHello offered the node-link ALPN (replayed
// from the listener's inspection). It owns conn once it returns nil.
func (m *Mesh) Accept(conn net.Conn) error {
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return ErrStopped
	}
	select {
	case m.handshakes <- struct{}{}:
	default:
		return ErrBusy
	}
	m.wg.Go(func() {
		defer func() { <-m.handshakes }()
		if err := m.accept(ctx, conn); err != nil {
			_ = conn.Close()
		}
	})
	return nil
}

func (m *Mesh) accept(ctx context.Context, raw net.Conn) error {
	h := &pendingHandshake{}
	ctx, done := m.track(ctx, h)
	defer done()
	conf := nodemesh.ServerTLS(m.cfg.Certificate, nodemesh.EdgeForwardALPN, m.peerSet)
	verify := conf.VerifyPeerCertificate
	conf.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
		if err := verify(raw, chains); err != nil {
			return err
		}
		leaf, err := x509.ParseCertificate(raw[0]) // verify parsed it already
		if err != nil {
			return nodemesh.ErrPeerUnknown
		}
		m.mu.Lock()
		h.pin = nodemesh.SPKIPin(leaf)
		m.mu.Unlock()
		return nil
	}
	conn := tls.Server(raw, conf)
	hctx, cancel := context.WithTimeout(ctx, m.cfg.DialTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		return err
	}
	peer, ok := nodemesh.PeerOf(conn.ConnectionState(), m.peerSet())
	if !ok {
		return nodemesh.ErrPeerUnknown
	}
	sess, err := yamux.Server(conn, m.yamuxConfig(), nil)
	if err != nil {
		return err
	}
	l := &link{peer: peer, sess: sess, remote: raw.RemoteAddr(), dialer: peer.NodeID, started: time.Now(), notAfter: certEnd(conn)}
	if _, installed := m.install(l); !installed {
		return errors.New("link not installed: peer already linked, no longer in the directory, or mesh stopped")
	}
	return nil
}

// install makes l the link to its peer, if the peer is still in the current directory with the pin, address and
// process epoch l was made with: the check runs under the lock applyPeers also takes, so a handshake that began
// before a peer was removed or rekeyed cannot install after. When both nodes dialled at once, the link the lower node id dialled wins on
// both sides, so the two nodes keep the same session.
func (m *Mesh) install(l *link) (*link, bool) {
	m.mu.Lock()
	if cur, ok := m.peerSet().Lookup(l.peer.NodeID); !ok || !peerMatches(cur, l.peer) {
		m.mu.Unlock()
		_ = l.sess.Close()
		return l, false
	}
	old := m.links[l.peer.NodeID]
	if old != nil && !old.sess.IsClosed() {
		lower := min(m.cfg.NodeID, l.peer.NodeID)
		if l.dialer != lower || old.dialer == lower {
			m.mu.Unlock()
			_ = l.sess.Close()
			return old, false
		}
	}
	if m.ctx == nil || m.ctx.Err() != nil {
		m.mu.Unlock()
		_ = l.sess.Close()
		return l, false
	}
	m.links[l.peer.NodeID] = l
	ctx := m.ctx
	m.wg.Go(func() { m.runLink(ctx, l) })
	m.mu.Unlock()
	if old != nil {
		_ = old.sess.Close()
	}
	return l, true
}

// runLink serves one link until it closes: its inbound streams and its outbound watch.
func (m *Mesh) runLink(ctx context.Context, l *link) {
	defer m.linkDown(l)
	if !l.notAfter.IsZero() { // a link never outlives its peer's certificate
		defer time.AfterFunc(time.Until(l.notAfter), func() { _ = l.sess.Close() }).Stop()
	}
	var streams sync.WaitGroup
	streams.Go(func() { m.sendWatch(ctx, l) })
	for {
		st, err := l.sess.AcceptStream()
		if err != nil {
			break
		}
		streams.Go(func() { m.serveStream(ctx, l, st) })
	}
	_ = l.sess.Close()
	streams.Wait()
}

func (m *Mesh) linkDown(l *link) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Only the current link withdraws the peer's names, and under the lock: a replacement installs (and starts its
	// watch) only after this returns, so its snapshot cannot be wiped by the old link's teardown.
	if m.links[l.peer.NodeID] == l {
		delete(m.links, l.peer.NodeID)
		m.dir.DropPeer(l.peer.NodeID)
	}
}
