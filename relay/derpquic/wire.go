package derpquic

import (
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"go4.org/mem"
	"tailscale.com/types/key"
)

const ALPN = "paperboat-derp-v1"
const MaxControl = 128 << 10
const MaxPacket = 2048
const fragmentPayload = 1024
const fragmentHeader = 43
const maxAssemblies = 64
const queueDepth = 64
const (
	frameAuth byte = 1 + iota
	frameReady
	frameControl
	framePing
	framePong
	framePacket
)

func writeFrame(w io.Writer, kind byte, b []byte) error {
	if len(b) > MaxControl {
		return ErrProtocol
	}
	var h [5]byte
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(b)))
	for _, p := range [][]byte{h[:], b} {
		for len(p) > 0 {
			n, e := w.Write(p)
			if e != nil {
				return e
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			p = p[n:]
		}
	}
	return nil
}
func readFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return 0, nil, e
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxControl {
		return 0, nil, ErrProtocol
	}
	b := make([]byte, n)
	_, e := io.ReadFull(r, b)
	return h[0], b, e
}

type packet struct {
	peer    key.NodePublic
	data    []byte
	control bool
}

func encodeControl(p packet) []byte {
	k := p.peer.Raw32()
	b := make([]byte, 32+len(p.data))
	copy(b, k[:])
	copy(b[32:], p.data)
	return b
}
func decodeControl(b []byte) (packet, error) {
	if len(b) < 33 || len(b) > 32+MaxPacket {
		return packet{}, ErrProtocol
	}
	var k [32]byte
	copy(k[:], b[:32])
	return packet{key.NodePublicFromRaw32(mem.B(k[:])), b[32:], true}, nil
}
func sendPacket(c *quic.Conn, sequence *atomic.Uint64, p packet) error {
	if len(p.data) == 0 || len(p.data) > MaxPacket {
		return ErrProtocol
	}
	id := sequence.Add(1)
	k := p.peer.Raw32()
	for off := 0; off < len(p.data); off += fragmentPayload {
		end := min(off+fragmentPayload, len(p.data))
		b := make([]byte, fragmentHeader+end-off)
		binary.BigEndian.PutUint64(b, id)
		copy(b[8:40], k[:])
		binary.BigEndian.PutUint16(b[40:42], uint16(len(p.data)))
		b[42] = byte(off / fragmentPayload)
		copy(b[43:], p.data[off:end])
		if err := c.SendDatagram(b); err != nil {
			return err
		}
	}
	return nil
}

type assembly struct {
	peer    key.NodePublic
	size    int
	parts   [2][]byte
	expires time.Time
}
type reassembler struct {
	pending   map[uint64]*assembly
	highest   uint64
	completed [128]uint64
}

func (r *reassembler) accept(b []byte, now time.Time, authorize func(key.NodePublic) bool) (packet, bool, error) {
	if len(b) < fragmentHeader+1 || len(b) > fragmentHeader+fragmentPayload {
		return packet{}, false, ErrProtocol
	}
	id := binary.BigEndian.Uint64(b)
	size := int(binary.BigEndian.Uint16(b[40:42]))
	index := int(b[42])
	var raw [32]byte
	copy(raw[:], b[8:40])
	peer := key.NodePublicFromRaw32(mem.B(raw[:]))
	if id == 0 || size < 1 || size > MaxPacket || index > 1 || index*fragmentPayload >= size || len(b)-fragmentHeader != min(fragmentPayload, size-index*fragmentPayload) {
		return packet{}, false, ErrProtocol
	}
	if authorize != nil && !authorize(peer) {
		return packet{}, false, nil
	}
	if id <= r.highest && r.highest-id >= 128 || r.completed[id%128] == id {
		return packet{}, false, nil
	}
	if id > r.highest {
		r.highest = id
	}
	if r.pending == nil {
		r.pending = make(map[uint64]*assembly)
	}
	for n, a := range r.pending {
		if !now.Before(a.expires) || n <= r.highest && r.highest-n >= 128 {
			delete(r.pending, n)
		}
	}
	a := r.pending[id]
	if a == nil {
		if len(r.pending) >= maxAssemblies {
			return packet{}, false, nil
		}
		a = &assembly{peer: peer, size: size, expires: now.Add(2 * time.Second)}
		r.pending[id] = a
	}
	if a.peer != peer || a.size != size {
		return packet{}, false, ErrProtocol
	}
	if a.parts[index] != nil {
		return packet{}, false, nil
	}
	a.parts[index] = append([]byte(nil), b[43:]...)
	if a.parts[0] == nil || size > fragmentPayload && a.parts[1] == nil {
		return packet{}, false, nil
	}
	data := make([]byte, 0, size)
	data = append(data, a.parts[0]...)
	data = append(data, a.parts[1]...)
	delete(r.pending, id)
	r.completed[id%128] = id
	return packet{peer, data, false}, true, nil
}

// budget bounds authenticated work, including fragments that never complete.
type budget struct {
	mu     sync.Mutex
	at     time.Time
	tokens float64
}

func (b *budget) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at.IsZero() {
		b.tokens = 256
	} else {
		b.tokens = min(256, b.tokens+now.Sub(b.at).Seconds()*4096)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
