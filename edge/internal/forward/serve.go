package forward

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// serveForward is the holder side of a forwarded visitor. The peer is authenticated by its link; the Open must
// name that peer, carry hop 1 and a key this node serves. The visitor's TLS then runs here, on this node.
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
	switch {
	case err != nil:
		refuse(nodemesh.RefusalBadRequest)
		return
	case open.Hop != nodemesh.ForwardHop:
		m.loopRefused.Add(1)
		m.outcome(OutcomeLoopRefused)
		refuse(nodemesh.RefusalLoop)
		return
	case open.Validate() != nil || open.From != l.peer.NodeID:
		refuse(nodemesh.RefusalBadRequest)
		return
	case !m.holds(open.Name):
		refuse(nodemesh.RefusalNotHolder)
		return
	}
	if l.active.Add(1) > int64(m.cfg.MaxStreamsPerPeer) {
		l.active.Add(-1)
		refuse(nodemesh.RefusalBusy)
		return
	}
	visitor := net.TCPAddrFromAddrPort(netip.MustParseAddrPort(open.Visitor))
	gate := make(chan struct{})
	conn := newStreamConn(st, l.remote, visitor, gate, func() { l.active.Add(-1) })
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
