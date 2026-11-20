package derpquic

import (
	"encoding/binary"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
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

// A mesh datagram names the destination as well as the source (a second 32-byte key), so a relay that holds neither
// can forward it: header 75 bytes + 1024 payload still fits one QUIC datagram. Bit 7 of the index byte marks a
// control packet (discovery), which the delivering relay authorizes by the network rule, not the pair rule.
const meshFragmentHeader = 75
const meshControlFlag = 0x80
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

// sendPacket sends p as one datagram when the carrier's path MTU allows it now (quic-go's path MTU discovery grows it; losing
// either of two fragments loses the whole packet, which doubled the loss the inner connection saw), and as fragments of
// fragmentPayload only when it does not fit. Prior art: quic-go Conn.SendDatagram reports DatagramTooLargeError instead of fragmenting.
func sendPacket(c *quic.Conn, sequence *atomic.Uint64, p packet) error {
	return sendFragments(c, sequence, p.peer, nil, p.data, false)
}

// sendFragments sends data as one datagram when the carrier's path MTU allows it, else as two fragments of fragmentPayload. A nil dst is the client wire
// (the destination is the peer the datagram is addressed to); a dst is a mesh forward from src to dst.
func sendFragments(c *quic.Conn, sequence *atomic.Uint64, src key.NodePublic, dst *key.NodePublic, data []byte, control bool) error {
	if len(data) == 0 || len(data) > MaxPacket {
		return ErrProtocol
	}
	id := sequence.Add(1)
	keys, header := src.AppendTo(nil), fragmentHeader
	if dst != nil {
		keys, header = dst.AppendTo(keys), meshFragmentHeader
	}
	datagram := func(off, end int) []byte {
		b := make([]byte, header+end-off)
		binary.BigEndian.PutUint64(b, id)
		copy(b[8:], keys)
		binary.BigEndian.PutUint16(b[header-3:], uint16(len(data)))
		b[header-1] = byte(off / fragmentPayload)
		if control {
			b[header-1] |= meshControlFlag
		}
		copy(b[header:], data[off:end])
		return b
	}
	if len(data) > fragmentPayload {
		err := c.SendDatagram(datagram(0, len(data)))
		var tooLarge *quic.DatagramTooLargeError
		if !errors.As(err, &tooLarge) {
			return err
		}
	}
	for off := 0; off < len(data); off += fragmentPayload {
		if err := c.SendDatagram(datagram(off, min(off+fragmentPayload, len(data)))); err != nil {
			return err
		}
	}
	return nil
}

type assembly struct {
	peer, dst key.NodePublic
	control   bool
	size      int
	parts     [2][]byte
	expires   time.Time
}
type reassembler struct {
	mesh      bool // fragments carry a destination key (meshFragmentHeader)
	pending   map[uint64]*assembly
	highest   uint64
	completed [128]uint64
}

func (r *reassembler) accept(b []byte, now time.Time, authorize func(key.NodePublic) bool) (packet, bool, error) {
	m, ok, err := r.acceptFragment(b, now, authorize)
	return m.packet, ok, err
}

// meshPacket is a reassembled mesh forward: the packet (peer = source route key) and its destination route key.
type meshPacket struct {
	packet
	dst key.NodePublic
}

func (r *reassembler) acceptFragment(b []byte, now time.Time, authorize func(key.NodePublic) bool) (meshPacket, bool, error) {
	header := fragmentHeader
	if r.mesh {
		header = meshFragmentHeader
	}
	if len(b) < header+1 || len(b) > header+MaxPacket {
		return meshPacket{}, false, ErrProtocol
	}
	id := binary.BigEndian.Uint64(b)
	size := int(binary.BigEndian.Uint16(b[header-3:]))
	index, control := int(b[header-1]), false
	if r.mesh {
		control, index = index&meshControlFlag != 0, index&^meshControlFlag
	}
	var raw, rawDst [32]byte
	copy(raw[:], b[8:40])
	peer := key.NodePublicFromRaw32(mem.B(raw[:]))
	var dst key.NodePublic
	if r.mesh {
		copy(rawDst[:], b[40:72])
		dst = key.NodePublicFromRaw32(mem.B(rawDst[:]))
	}
	part := len(b) - header
	// A packet is one datagram carrying all of it (index 0), or two fragments of fragmentPayload and the rest.
	whole := index == 0 && part == size
	if id == 0 || size < 1 || size > MaxPacket || index > 1 || index*fragmentPayload >= size || !whole && part != min(fragmentPayload, size-index*fragmentPayload) {
		return meshPacket{}, false, ErrProtocol
	}
	if authorize != nil && !authorize(peer) {
		return meshPacket{}, false, nil
	}
	if id <= r.highest && r.highest-id >= 128 || r.completed[id%128] == id {
		return meshPacket{}, false, nil
	}
	if id > r.highest {
		r.highest = id
	}
	if whole {
		r.completed[id%128] = id
		return meshPacket{packet{peer, append([]byte(nil), b[header:]...), control}, dst}, true, nil
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
			return meshPacket{}, false, nil
		}
		a = &assembly{peer: peer, dst: dst, control: control, size: size, expires: now.Add(2 * time.Second)}
		r.pending[id] = a
	}
	if a.peer != peer || a.dst != dst || a.control != control || a.size != size {
		return meshPacket{}, false, ErrProtocol
	}
	if a.parts[index] != nil {
		return meshPacket{}, false, nil
	}
	a.parts[index] = append([]byte(nil), b[header:]...)
	if a.parts[0] == nil || size > fragmentPayload && a.parts[1] == nil {
		return meshPacket{}, false, nil
	}
	data := make([]byte, 0, size)
	data = append(data, a.parts[0]...)
	data = append(data, a.parts[1]...)
	delete(r.pending, id)
	r.completed[id%128] = id
	return meshPacket{packet{peer: peer, data: data, control: control}, dst}, true, nil
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
	offered uint64 // bytes offered to reserve since the last takeDemand
	held    uint64 // of them, the bytes the limit delayed or dropped
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
	b.offered += uint64(size)
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
	// The limit holds this packet back: the flow wants more than it gets, which the demand reports (relayshare).
	b.held += uint64(size)
	delay := time.Duration((cost - b.tokens) / b.rate * float64(time.Second))
	if delay > maxDelay {
		return 0, false
	}
	b.tokens -= cost
	return delay, true
}

// AccountDemand is what an account offered a relay over a window, in bytes, and the part of it the relay's speed limit delayed or dropped.
type AccountDemand struct{ Offered, Held uint64 }

// takeDemand returns the demand since the last call and starts a new window.
func (b *budget) takeDemand() AccountDemand {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := AccountDemand{b.offered, b.held}
	b.offered, b.held = 0, 0
	return d
}

// wireRate is the budget rate, in bytes per second, for an account whose plan speed is rate: grossed up by relaylimits.AccountWireOverhead, never
// above the ceiling of a relay.
func wireRate(rate float64) float64 {
	return min(rate*relaylimits.AccountWireOverhead, accountBytesPerSecond)
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
