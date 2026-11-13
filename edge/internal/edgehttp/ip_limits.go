package edgehttp

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

// addressRate is one token bucket's refill per second and burst.
type addressRate struct{ perSecond, burst float64 }

var (
	// One address's HTTP requests (any number of links) and new WebSocket upgrades on this edge.
	addressRequestRate = addressRate{perSecond: 100, burst: 1000}
	addressUpgradeRate = addressRate{perSecond: 20, burst: 40}
	// One address's TLS handshakes on the shared 443 listener.
	addressHandshakeRate = addressRate{perSecond: 20, burst: 50}
)

const (
	// addressStreams bounds one address's requests in flight (WebSockets and SSE included).
	addressStreams     = 256
	addressIdle        = 2 * time.Minute
	addressShards      = 64
	addressShardKeys   = 16384 // 1M addresses per edge in all, about 100 MB at most
	addressSweepPeriod = 30 * time.Second
)

type addressBucket struct {
	tokens float64
	at     time.Time
}

func (b *addressBucket) take(now time.Time, r addressRate) bool {
	if b.at.IsZero() {
		b.tokens = r.burst
	} else {
		b.tokens = min(r.burst, b.tokens+now.Sub(b.at).Seconds()*r.perSecond)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type addressState struct {
	opens, upgrades addressBucket
	active          int
	last            time.Time
}

type addressShard struct {
	mu    sync.Mutex
	keys  map[string]*addressState
	swept time.Time
}

// addressLimiter is one edge-wide table of address limits.
type addressLimiter struct {
	opens, upgrades addressRate
	streams         int // 0: no in-flight bound
	seed            maphash.Seed
	shards          [addressShards]addressShard
	now             func() time.Time
}

func newAddressLimiter(opens, upgrades addressRate, streams int) *addressLimiter {
	l := &addressLimiter{opens: opens, upgrades: upgrades, streams: streams, seed: maphash.MakeSeed(), now: time.Now}
	for i := range l.shards {
		l.shards[i].keys = make(map[string]*addressState)
	}
	return l
}

// addressKey is the limit key for a client address: the IPv4 address, or the IPv6 /64.
func addressKey(address string) string {
	ip := net.ParseIP(address)
	if ip == nil {
		return address
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// admit takes one open (and one upgrade for a WebSocket) for the address; release ends it. A
// refusal leaves the address's counters as they were.
func (l *addressLimiter) admit(address string, upgrade bool) (release func(), ok bool) {
	key := addressKey(address)
	shard := &l.shards[maphash.String(l.seed, key)%addressShards]
	now := l.now()
	shard.mu.Lock()
	defer shard.mu.Unlock()
	state := shard.keys[key]
	if state == nil {
		if now.Sub(shard.swept) >= addressSweepPeriod {
			shard.sweep(now)
		}
		if len(shard.keys) >= addressShardKeys {
			// shortcut: a flood from over 1M addresses is past what per-address limits can stop; new
			// addresses pass untracked until idle ones expire (provider scrubbing and nftables still apply).
			return func() {}, true
		}
		state = &addressState{}
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

func (s *addressShard) sweep(now time.Time) {
	s.swept = now
	for key, state := range s.keys {
		if state.active == 0 && now.Sub(state.last) >= addressIdle {
			delete(s.keys, key)
		}
	}
}
