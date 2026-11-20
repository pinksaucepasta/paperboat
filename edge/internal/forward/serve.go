package forward

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// admitter is what the Injector offers for a hostname key: the server name inside a ClientHello (the edgehttp
// inspection) and whether this node can serve that name now, answered before the visitor is acknowledged.
type admitter interface {
	ServerName(hello []byte) (string, error)
	Admit(name string) error
}

// serveForward is the holder side of a forwarded visitor. The peer is authenticated by its link and must still be in
// the current peer directory; the Open must name that peer, carry hop 1 and a key this node serves. A hostname Open
// carries the visitor's ClientHello: the node acks only after its SNI equals the key and the listener's own
// authority says it serves the name, so a refusal still reaches the forwarder in time to try the next holder.
// The visitor address is trusted as the client address for the owner's IP rules only because it comes from a peer
// that passed both checks and is a routable address (attestedVisitor); the forwarder takes it from the visitor
// socket, never from a header.
func (m *Mesh) serveForward(ctx context.Context, l *link, st *yamux.Stream) {
	var open nodemesh.Open
	_ = st.SetReadDeadline(time.Now().Add(m.cfg.OpenTimeout))
	err := nodemesh.ReadMessage(st, &open)
	_ = st.SetReadDeadline(time.Time{})
	refuse := func(reason string) {
		_ = st.SetWriteDeadline(time.Now().Add(writeDeadline))
		_ = nodemesh.WriteMessage(st, nodemesh.OpenAck{Refusal: reason})
		_ = st.Close()
	}
	if err != nil {
		refuse(nodemesh.RefusalBadRequest)
		return
	}
	if open.Hop != nodemesh.ForwardHop {
		m.loopRefused.Add(1)
		m.outcome(OutcomeLoopRefused)
		refuse(nodemesh.RefusalLoop)
		return
	}
	visitor, ok := m.attestedVisitor(l, open.Visitor)
	if open.Validate() != nil || open.From != l.peer.NodeID || !ok || strings.HasPrefix(open.Name, nodemesh.WildcardPrefix) {
		refuse(nodemesh.RefusalBadRequest)
		return
	}
	// The link outlives a directory change: the peer must be in the set as it is now, not only at the handshake.
	if !m.PeerCurrent(l.peer) {
		refuse(nodemesh.RefusalNotHolder)
		return
	}
	if err := m.admitName(open); err != nil {
		refuse(refusalFor(err))
		return
	}
	if l.active.Add(1) > int64(m.cfg.MaxStreamsPerPeer) {
		l.active.Add(-1)
		refuse(nodemesh.RefusalBusy)
		return
	}
	gate := make(chan struct{})
	conn := newStreamConn(st, attestedAddr{l.remote}, visitor, gate, func() { l.active.Add(-1) })
	if err := m.inject(conn, open.Name, visitor); err != nil {
		refuse(refusalFor(err))
		_ = conn.Close() // releases the per-peer slot
		return
	}
	_ = st.SetWriteDeadline(time.Now().Add(writeDeadline))
	if err := nodemesh.WriteMessage(st, nodemesh.OpenAck{OK: true}); err != nil {
		_ = conn.Close()
		return
	}
	_ = st.SetWriteDeadline(time.Time{})
	m.accepted.Add(1)
	close(gate)
}

// attestedAddr is the local address of a forwarded visitor's connection. Its RemoteAddr was attested by the link, so
// the HTTP layer reads the client address from it and never from forwarding headers (edgehttp.Policy.clientIP).
type attestedAddr struct{ net.Addr }

// AttestedVisitor marks the connection whose RemoteAddr a verified peer attested.
func (attestedAddr) AttestedVisitor() {}

// admitName is the holder's pre-ack decision for the Open's key: nil, or why this node will not serve it.
func (m *Mesh) admitName(open nodemesh.Open) error {
	if _, tcp := nodemesh.ParseTCPKey(open.Name); tcp {
		if !m.holds(open.Name) {
			return errNotHeld
		}
		return nil
	}
	a, ok := m.cfg.Injector.(admitter)
	if !ok || len(open.Hello) == 0 {
		return errNotHeld
	}
	if sni, err := a.ServerName(open.Hello); err != nil || sni != open.Name {
		return errNotHeld
	}
	return a.Admit(open.Name)
}

var errNotHeld = errors.New("this node does not hold the name")

// attestedVisitor validates the visitor address a peer claims: a unicast address that is neither this link's nor the
// forwarder's own (those name the nodes, never a visitor). IPv4-mapped addresses are normalised.
func (m *Mesh) attestedVisitor(l *link, claimed string) (*net.TCPAddr, bool) {
	ap, err := netip.ParseAddrPort(claimed)
	if err != nil || ap.Port() == 0 {
		return nil, false
	}
	ip := ap.Addr().Unmap().WithZone("")
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return nil, false
	}
	for _, own := range []net.Addr{l.remote, l.sess.LocalAddr()} {
		if own == nil {
			continue
		}
		if o, err := netip.ParseAddrPort(own.String()); err == nil && o.Addr().Unmap() == ip {
			return nil, false
		}
	}
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(ip, ap.Port())), true
}

func (m *Mesh) inject(conn net.Conn, key string, visitor net.Addr) error {
	if port, ok := nodemesh.ParseTCPKey(key); ok {
		if m.cfg.TCPInjector == nil {
			return errors.New("raw TCP forwarding is not available on this node")
		}
		return m.cfg.TCPInjector.InjectTCP(conn, port)
	}
	return m.cfg.Injector.Inject(conn, visitor)
}

func refusalFor(err error) string {
	switch {
	case errors.Is(err, ErrDraining):
		return nodemesh.RefusalDraining
	case errors.Is(err, ErrBusy):
		return nodemesh.RefusalBusy
	}
	return nodemesh.RefusalNotHolder
}
