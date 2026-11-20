package forward

import (
	"context"
	"crypto/tls"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// pendingHandshake is a dial or accept in flight. A dial knows its peer up front; an accept learns the pin when the
// peer's certificate verified (pin is set under Mesh.mu from then on).
type pendingHandshake struct {
	peer   nodemesh.Peer // dials: the directory entry dialled
	pin    string        // accepts: the verified certificate pin
	dial   bool
	cancel context.CancelFunc
}

// stale reports whether set no longer holds the peer this handshake is with, so the handshake must be cut.
func (h *pendingHandshake) stale(set nodemesh.PeerSet) bool {
	if h.dial {
		cur, ok := set.Lookup(h.peer.NodeID)
		return !ok || !peerMatches(cur, h.peer)
	}
	if h.pin == "" {
		return false // the peer is not known yet; install revalidates it
	}
	_, ok := set.ByPin(h.pin)
	return !ok
}

// track registers h and returns a context that applyPeers cancels when the peer leaves the directory or changes.
// The returned func unregisters h.
func (m *Mesh) track(ctx context.Context, h *pendingHandshake) (context.Context, func()) {
	ctx, h.cancel = context.WithCancel(ctx)
	m.mu.Lock()
	m.pending[h] = struct{}{}
	m.mu.Unlock()
	return ctx, func() {
		m.mu.Lock()
		delete(m.pending, h)
		m.mu.Unlock()
		h.cancel()
	}
}

// certEnd is when the peer's certificate stops being valid; zero when the handshake carried none.
func certEnd(conn *tls.Conn) time.Time {
	if certs := conn.ConnectionState().PeerCertificates; len(certs) > 0 {
		return certs[0].NotAfter
	}
	return time.Time{}
}
