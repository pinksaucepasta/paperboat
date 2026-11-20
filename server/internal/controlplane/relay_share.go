package controlplane

import (
	"sync"
	"time"
)

// relayDemandBook holds, per relay node, the bytes per second each connected account offered it in the node's last observation (the
// relays' demand), in memory: it changes every observation and is valid for one lease. A server restart leaves each relay alone for
// the few seconds until the others report, so it briefly gets the account's full speed.
type relayDemandBook struct {
	mu     sync.Mutex
	byNode map[string]relayNodeDemand
}

type relayNodeDemand struct {
	at      time.Time
	account map[string]float64
}

// record replaces the node's demand and forgets nodes that have not reported for a lease.
func (b *relayDemandBook) record(node string, demand map[string]float64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byNode == nil {
		b.byNode = map[string]relayNodeDemand{}
	}
	for n, d := range b.byNode {
		if now.Sub(d.at) >= relayNodeLease {
			delete(b.byNode, n)
		}
	}
	b.byNode[node] = relayNodeDemand{at: now, account: demand}
}

// ratesOf is what each live relay offered for the account: node -> bytes per second (only relays that report the account).
func (b *relayDemandBook) ratesOf(account string, now time.Time) map[string]float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]float64{}
	for n, d := range b.byNode {
		if rate, ok := d.account[account]; ok && now.Sub(d.at) < relayNodeLease {
			out[n] = rate
		}
	}
	return out
}
