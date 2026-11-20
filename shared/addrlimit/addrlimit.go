// Package addrlimit bounds one client address (IPv4 whole, IPv6 /64): a token-bucket rate and a count of
// connections or requests in flight. It never bounds the node; capacity is watched by resource alerts.
package addrlimit

import (
	"hash/maphash"
	"net"
	"sync"
	"time"
)

// Edge-wide limits per client address (edge abuse plan, layers L2 and L3): the flood floor. A
// flood comes from a few addresses at abnormal rates; a popular link comes from many addresses
// that each behave normally, so limits keyed by address stop the first and never the second.
// IPv4 addresses are keyed whole, IPv6 by /64 (one host's usual allocation). Prior art: zrok's
// public frontend limits per client IP; ngrok's rate-limit action keyed on conn.client_ip.

// Rate is one token bucket's refill per second and burst.
type Rate struct{ PerSecond, Burst float64 }

const (
	// Streams bounds one address's requests or connections in flight (WebSockets and SSE included).
	Streams      = 256
	idle         = 2 * time.Minute
	shardCount   = 64
	maxShardKeys = 16384 // 1M addresses per edge in all, about 100 MB at most
	sweepPeriod  = 30 * time.Second
)

type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) take(now time.Time, r Rate) bool {
	if b.at.IsZero() {
		b.tokens = r.Burst
	} else {
		b.tokens = min(r.Burst, b.tokens+now.Sub(b.at).Seconds()*r.PerSecond)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type addrState struct {
	opens, upgrades bucket
	active          int
	last            time.Time
}

type addrShard struct {
	mu    sync.Mutex
	keys  map[string]*addrState
	swept time.Time
}

// Limiter is one table of address limits.
type Limiter struct {
	opens, upgrades Rate
	streams         int // 0: no in-flight bound
	shardKeys       int // addresses tracked per shard
	seed            maphash.Seed
	shards          [shardCount]addrShard
	now             func() time.Time
}

// NewInFlight bounds only how many connections one address holds at once: its token buckets never run dry.
func NewInFlight(streams int) *Limiter {
	unlimited := Rate{PerSecond: 1e18, Burst: 1e18}
	return New(unlimited, unlimited, streams)
}

// SetClock replaces the clock (tests).
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

// New returns a limiter with the open and upgrade rates and an in-flight bound (0: none).
func New(opens, upgrades Rate, streams int) *Limiter {
	l := &Limiter{opens: opens, upgrades: upgrades, streams: streams, shardKeys: maxShardKeys, seed: maphash.MakeSeed(), now: time.Now}
	for i := range l.shards {
		l.shards[i].keys = make(map[string]*addrState)
	}
	return l
}

// Key is the limit key for a client address: the IPv4 address, or the IPv6 /64.
func Key(address string) string {
	ip := net.ParseIP(address)
	if ip == nil {
		return address
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// Admit takes one open (and one upgrade for a WebSocket) for the address; release ends it. A
// refusal leaves the address's counters as they were.
func (l *Limiter) Admit(address string, upgrade bool) (release func(), ok bool) {
	key := Key(address)
	shard := &l.shards[maphash.String(l.seed, key)%shardCount]
	now := l.now()
	shard.mu.Lock()
	defer shard.mu.Unlock()
	state := shard.keys[key]
	if state == nil {
		if now.Sub(shard.swept) >= sweepPeriod {
			shard.sweep(now)
		}
		if len(shard.keys) >= l.shardKeys {
			// shortcut: a flood from over 1M addresses is past what per-address limits can stop; new
			// addresses pass untracked until idle ones expire (provider scrubbing and nftables still apply).
			return func() {}, true
		}
		state = &addrState{}
		shard.keys[key] = state
	}
	state.last = now
	if l.streams > 0 && state.active >= l.streams {
		return nil, false
	}
	if upgrade {
		saved := state.upgrades
		if !state.upgrades.take(now, l.upgrades) {
			return nil, false
		}
		if !state.opens.take(now, l.opens) {
			state.upgrades = saved
			return nil, false
		}
	} else if !state.opens.take(now, l.opens) {
		return nil, false
	}
	if l.streams == 0 {
		return func() {}, true
	}
	state.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			shard.mu.Lock()
			state.active--
			state.last = l.now()
			shard.mu.Unlock()
		})
	}, true
}

func (s *addrShard) sweep(now time.Time) {
	s.swept = now
	for key, state := range s.keys {
		if state.active == 0 && now.Sub(state.last) >= idle {
			delete(s.keys, key)
		}
	}
}
