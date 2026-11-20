package forward

import (
	"context"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// Holders lists, best first, the peers that serve key. The listener forwards only when this is not empty.
func (m *Mesh) Holders(key string) []string {
	holders := m.dir.Holders(key)
	ids := make([]string, len(holders))
	for i, h := range holders {
		ids[i] = h.NodeID
	}
	return ids
}

// Forward passes the visitor on conn, which must replay the bytes already read from it (its ClientHello), to a node
// that serves key. ctx bounds only the opening: holders are tried in order (not draining, same region, id), then
// once more after a forced refresh of the server's holder snapshot. The returned error is a *Failure when no holder
// took the visitor; no byte has then reached the visitor, so the caller may still answer it. Once a holder took the
// visitor Forward copies until either side ends, whatever ctx does (a drain must not cut forwarded visitors; only
// Stop does), and returns nil. The caller closes conn.
func (m *Mesh) Forward(ctx context.Context, conn net.Conn, key string) error {
	m.mu.Lock()
	life := m.ctx
	m.mu.Unlock()
	if life == nil {
		return &Failure{Stage: StageLookup, Err: ErrStopped}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*(m.cfg.DialTimeout+m.cfg.OpenTimeout))
	defer cancel()
	visitor := conn.RemoteAddr().String()
	tried := map[string]bool{}
	var last *Failure
	for round := range 2 {
		if round == 1 {
			m.refreshSeed(ctx, true)
		}
		for _, h := range m.dir.Holders(key) {
			if tried[h.NodeID] || ctx.Err() != nil {
				continue
			}
			tried[h.NodeID] = true
			stream, failure := m.openStream(ctx, h.NodeID, key, visitor)
			if failure != nil {
				last = failure
				continue
			}
			if stream == nil {
				last = &Failure{Stage: StageOpen, Err: ErrUnavailable}
				continue
			}
			m.forwarded.Add(1)
			m.outcome(OutcomeForwarded)
			pipe(life, conn, stream, &m.bytesOut, &m.bytesIn)
			_ = stream.Close()
			return nil
		}
	}
	if last == nil {
		m.holderMiss.Add(1)
		m.outcome(OutcomeHolderMiss)
		return &Failure{Stage: StageLookup, Err: ErrNoHolder}
	}
	return last
}

// openStream opens a forward stream to node id and waits for its ack. It returns the stream when the holder took
// the visitor, a nil stream and nil failure when the holder refused (the refusal is counted), and a failure when
// the node could not be reached or did not answer.
func (m *Mesh) openStream(ctx context.Context, id, key, visitor string) (*streamConn, *Failure) {
	l, err := m.linkTo(ctx, id)
	if err != nil {
		m.unreachable.Add(1)
		m.outcome(OutcomeUnreachable)
		return nil, &Failure{Stage: StageDial, Err: err}
	}
	octx, cancel := context.WithTimeout(ctx, m.cfg.OpenTimeout)
	defer cancel()
	st, err := l.sess.OpenStream(octx)
	if err != nil {
		m.unreachable.Add(1)
		m.outcome(OutcomeUnreachable)
		return nil, &Failure{Stage: StageOpen, Err: err}
	}
	deadline := time.Now().Add(m.cfg.OpenTimeout)
	_ = st.SetDeadline(deadline)
	var ack nodemesh.OpenAck
	err = writeStreamKind(st, streamForward)
	if err == nil {
		err = nodemesh.WriteMessage(st, nodemesh.Open{V: 1, Name: key, Visitor: visitor, From: m.cfg.NodeID, Hop: nodemesh.ForwardHop})
	}
	if err == nil {
		err = nodemesh.ReadMessage(st, &ack)
	}
	if err != nil {
		_ = st.Reset()
		m.unreachable.Add(1)
		m.outcome(OutcomeUnreachable)
		return nil, &Failure{Stage: StageOpen, Err: err}
	}
	_ = st.SetDeadline(time.Time{})
	if ack.OK {
		return newStreamConn(st, nil, l.remote, nil, nil), nil
	}
	_ = st.Close()
	switch ack.Refusal {
	case nodemesh.RefusalNotHolder:
		m.holderMiss.Add(1)
		m.outcome(OutcomeHolderMiss)
		m.dir.Refused(key, id)
	case nodemesh.RefusalLoop:
		m.loopRefused.Add(1)
		m.outcome(OutcomeLoopRefused)
	default: // draining, busy, bad_request: this holder is not used for this visitor
		m.outcome(OutcomeRefused)
	}
	return nil, nil
}

// linkTo returns the live link to node id, dialling it when none is up.
func (m *Mesh) linkTo(ctx context.Context, id string) (*link, error) {
	if l := m.liveLink(id); l != nil {
		return l, nil
	}
	peer, ok := m.peerSet().Lookup(id)
	if !ok {
		return nil, errUnknownPeer
	}
	return m.dial(ctx, peer)
}

// TCPPorts lists the public raw TCP ports some peer holds a tunnel for: the ports this node listens on too, to
// forward the visitors who dial them here.
func (m *Mesh) TCPPorts() []uint16 { return m.dir.TCPPorts() }
