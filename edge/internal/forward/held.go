package forward

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/libp2p/go-yamux/v5"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// heldSnapshot is the immutable set of keys this node serves; a new one replaces it only when the set changed.
type heldSnapshot struct {
	set  map[string]struct{}
	hash uint64 // order-independent digest of the keys, to see a change without comparing sets
}

// refreshHeld reads the node's current holdings. It is cheap (a few thousand names) and runs every 250 ms and when
// a peer asks for a name the snapshot lacks, so a name that just attached is served at once.
func (m *Mesh) refreshHeld() {
	m.heldMu.Lock()
	defer m.heldMu.Unlock()
	m.heldAt = time.Now()
	keys := m.cfg.Held.Held()
	set := make(map[string]struct{}, len(keys))
	var digest uint64
	for _, key := range keys {
		if _, dup := set[key]; dup || nodemesh.ValidKey(key) != nil {
			continue
		}
		set[key] = struct{}{}
		h := fnv.New64a()
		_, _ = h.Write([]byte(key))
		digest ^= h.Sum64()
	}
	if cur := m.held.Load(); cur != nil && cur.hash == digest && len(cur.set) == len(set) {
		return
	}
	m.held.Store(&heldSnapshot{set: set, hash: digest})
}

// heldRecent is how fresh a read of the holdings is taken to be for a forward's check.
const heldRecent = 5 * time.Millisecond

func (m *Mesh) heldLoop(ctx context.Context) {
	ticks := 0
	for sleep(ctx, m.cfg.HeldInterval) {
		m.refreshHeld()
		if ticks++; ticks%40 == 0 {
			m.dir.Sweep()
		}
	}
}

// holds reports whether this node serves key. It re-reads its holdings at most every 5 ms (so a burst of forwards
// costs one read), which makes a name that just attached served at once and one that just left refused at once: the
// watch tick has not told the peers yet.
func (m *Mesh) holds(key string) bool {
	m.heldMu.Lock()
	recent := time.Since(m.heldAt) < heldRecent
	m.heldMu.Unlock()
	if !recent {
		m.refreshHeld()
	}
	_, ok := m.held.Load().set[key]
	return ok
}

// sendWatch streams this node's holdings to the peer: a chunked snapshot, then add/remove deltas.
func (m *Mesh) sendWatch(ctx context.Context, l *link) {
	octx, cancel := context.WithTimeout(ctx, m.cfg.OpenTimeout)
	st, err := l.sess.OpenStream(octx)
	cancel()
	if err != nil {
		_ = l.sess.Close()
		return
	}
	defer st.Close()
	sent := m.held.Load()
	err = writeStreamKind(st, streamWatch)
	if err == nil {
		err = writeWatch(st, nodemesh.WatchSnapshot, keysOf(sent.set, nil), true)
	}
	if err == nil {
		err = writeWatch(st, nodemesh.WatchEnd, nil, false)
	}
	for err == nil {
		if !sleep(ctx, m.cfg.HeldInterval) {
			return
		}
		select {
		case <-l.sess.CloseChan():
			return
		default:
		}
		cur := m.held.Load()
		if cur == sent {
			continue
		}
		if err = writeWatch(st, nodemesh.WatchAdd, keysOf(cur.set, sent.set), false); err == nil {
			err = writeWatch(st, nodemesh.WatchRemove, keysOf(sent.set, cur.set), false)
		}
		sent = cur
	}
	_ = l.sess.Close()
}

// keysOf lists the keys of in that are not in except.
func keysOf(in, except map[string]struct{}) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		if _, skip := except[key]; !skip {
			out = append(out, key)
		}
	}
	return out
}

func writeStreamKind(st *yamux.Stream, kind byte) error {
	_ = st.SetWriteDeadline(time.Now().Add(writeDeadline))
	_, err := st.Write([]byte{kind})
	return err
}

// watchFrameBytes keeps a frame well under nodemesh.MaxFrame whatever the names' length.
const watchFrameBytes = 48 << 10

// writeWatch sends names as frames of kind; more marks snapshot frames that a WatchEnd will close. An empty list
// sends nothing except for kind WatchEnd.
func writeWatch(st *yamux.Stream, kind string, names []string, more bool) error {
	send := func(batch []string) error {
		_ = st.SetWriteDeadline(time.Now().Add(writeDeadline))
		return nodemesh.WriteMessage(st, nodemesh.Watch{Kind: kind, Names: batch, More: more})
	}
	if kind == nodemesh.WatchEnd {
		return send(nil)
	}
	start, size := 0, 0
	for i, name := range names {
		if i-start == nodemesh.MaxNamesPerFrame || size+len(name)+4 > watchFrameBytes {
			if err := send(names[start:i]); err != nil {
				return err
			}
			start, size = i, 0
		}
		size += len(name) + 4
	}
	if start < len(names) {
		return send(names[start:])
	}
	return nil
}

// serveStream handles one stream the peer opened: the first byte says what it is.
func (m *Mesh) serveStream(ctx context.Context, l *link, st *yamux.Stream) {
	var kind [1]byte
	_ = st.SetReadDeadline(time.Now().Add(m.cfg.OpenTimeout))
	if _, err := st.Read(kind[:]); err != nil {
		_ = st.Reset()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	switch kind[0] {
	case streamWatch:
		m.serveWatch(l, st)
	case streamForward:
		m.serveForward(ctx, l, st)
	default:
		_ = st.Reset()
	}
}

// serveWatch applies the peer's holdings to the directory until the stream or link ends. A peer that sends a
// malformed frame, or claims more names than a node can hold, loses its link.
func (m *Mesh) serveWatch(l *link, st *yamux.Stream) {
	for {
		var w nodemesh.Watch
		if err := nodemesh.ReadMessage(st, &w); err != nil {
			break
		}
		if w.Validate() != nil || m.dir.ApplyWatch(l.peer.NodeID, w) != nil {
			_ = l.sess.Close()
			return
		}
	}
	_ = st.Close()
}
