package derpquic

import (
	"encoding/binary"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/quic-go/quic-go"
	"go4.org/mem"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=keyed-packet-wire-format
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
	frameAuthGeneration
	frameAuthBatch
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
	return append(p.peer.AppendTo(make([]byte, 0, 32+len(p.data))), p.data...)
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
	k := p.peer.AppendTo(nil)
	for off := 0; off < len(p.data); off += fragmentPayload {
		end := min(off+fragmentPayload, len(p.data))
		b := make([]byte, fragmentHeader+end-off)
		binary.BigEndian.PutUint64(b, id)
		copy(b[8:40], k)
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

// Forwarding budgets bound authenticated work per carrier and per account. Every
// datagram or frame costs its size, at least budgetMinimumCost, so incomplete
// fragments and tiny frames are bounded too.
const (
	carrierBytesPerSecond = 64 << 20  // 512 Mbit/s per carrier
	accountBytesPerSecond = 128 << 20 // the ceiling of an account's carriers on one relay; the fair-use cap is lower
	// MaxAccountMbit bounds the configured caps: the relay's own ceiling.
	MaxAccountMbit    = float64(accountBytesPerSecond) * 8 / 1e6
	budgetBurstBytes  = 4 << 20
	budgetMinimumCost = fragmentHeader + fragmentPayload
	// accountMinimumCost is the least an account's budget charges a datagram or frame. The account budget is a BANDWIDTH cap, so it
	// counts bytes: charging every datagram at least a full fragment (1067 B) made a 60-byte QUIC ACK cost as much as a data packet and a
	// 1252-byte inner packet (two fragments) cost 2134, so a QUIC transfer got about half the account's speed (4.7 of 10, 52 of 100 Mbit/s,
	// 0.26 of 1) and its ACKs shared the bucket with its data. The carrier budget keeps budgetMinimumCost: it bounds the work an
	// authenticated peer can ask for (tiny frames, incomplete fragments).
	accountMinimumCost = 64
)

type budget struct {
	mu      sync.Mutex
	minCost float64 // the least a datagram or frame is charged; zero means budgetMinimumCost
	rate    float64
	burst   float64
	at      time.Time
	tokens  float64
}

func newBudget(bytesPerSecond float64) *budget {
	return newBudgetBurst(bytesPerSecond, budgetBurstBytes)
}

func newBudgetBurst(bytesPerSecond, burst float64) *budget {
	return &budget{rate: bytesPerSecond, burst: burst}
}

// newAccountBudget is an account's speed budget: it charges bytes (accountMinimumCost at least).
func newAccountBudget(bytesPerSecond float64) *budget {
	return &budget{rate: bytesPerSecond, burst: accountBurst(bytesPerSecond), minCost: accountMinimumCost}
}

// charge is what a datagram or frame of size bytes costs this budget.
func (b *budget) charge(size int) float64 {
	minimum := b.minCost
	if minimum == 0 {
		minimum = budgetMinimumCost
	}
	return max(float64(size), minimum)
}

// accountBurst is the burst of an account's budget: a quarter second of its speed (at least what carries one packet), so the total an
// account moves in a window stays within its speed plus that, and a capped account does not start with seconds of free traffic.
func accountBurst(bytesPerSecond float64) float64 {
	return min(float64(budgetBurstBytes), max(bytesPerSecond/4, 2*budgetMinimumCost))
}

// shapingMaxDelay bounds how long an account's over-cap traffic may wait in the relay: past it a datagram is dropped (a full buffer),
// so latency stays bounded while the account's own transport converges on its speed instead of collapsing under drops.
const shapingMaxDelay = 500 * time.Millisecond

// reserve takes size bytes from the budget the way a shaper does: it returns how long the caller must wait before forwarding them (zero
// within the burst), or !ok when that wait would exceed maxDelay (the buffer is full). Tokens go negative while packets wait.
// Prior art: Tailscale's DERP delays a client's sends past its limit rather than dropping them (derp/derpserver/derpserver.go
// rateLimitWait: x/time/rate ReserveN and a sleep), so the sender's TCP slows to the limit; here the relay carries datagrams, so the
// delay is a bounded queue (readDatagrams) or a sleep before the next frame is read (the stream loop, TCP back-pressure).
func (b *budget) reserve(now time.Time, size int, maxDelay time.Duration) (time.Duration, bool) {
	cost := b.charge(size)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at.IsZero() {
		b.tokens = b.burst
	} else {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.at).Seconds()*b.rate)
	}
	b.at = now
	if b.tokens >= cost {
		b.tokens -= cost
		return 0, true
	}
	delay := time.Duration((cost - b.tokens) / b.rate * float64(time.Second))
	if delay > maxDelay {
		return 0, false
	}
	b.tokens -= cost
	return delay, true
}

// setRate changes the refill rate and the burst; tokens already held are cut to the new burst.
func (b *budget) setRate(bytesPerSecond, burst float64) {
	b.mu.Lock()
	b.rate, b.burst = bytesPerSecond, burst
	b.tokens = min(b.tokens, burst)
	b.mu.Unlock()
}

func (b *budget) allow(now time.Time, size int) bool {
	cost := b.charge(size)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at.IsZero() {
		b.tokens = b.burst
	} else {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.at).Seconds()*b.rate)
	}
	b.at = now
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// MaxGrantParts bounds the grants one credential carries: an endpoint whose peers do not fit one grant
// gets one grant per chunk of peers (with at most 64 peers each), joined by newlines.
const MaxGrantParts = peernetwork.MaxGrantParts

// grantParts splits a bounded relay credential into its independently signed parts.
func grantParts(credential string) ([]string, error) {
	parts := strings.Split(credential, "\n")
	if credential == "" || len(parts) > MaxGrantParts {
		return nil, ErrAdmission
	}
	for _, part := range parts {
		if part == "" || len(part) > MaxControl {
			return nil, ErrAdmission
		}
	}
	return parts, nil
}
