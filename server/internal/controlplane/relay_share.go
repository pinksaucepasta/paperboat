package controlplane

import (
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relayshare"
)

// relayDemandBook holds, per relay node, what each connected account offered it and what the limit held back, in bytes per second, in
// the node's last observation (the relays' demand), in memory: it changes every observation and is valid for one lease. A server restart leaves each relay alone for
// the few seconds until the others report, so it briefly gets the account's full speed.
type relayDemandBook struct {
	mu     sync.Mutex
	byNode map[string]relayNodeDemand
}

type relayNodeDemand struct {
	at      time.Time
	account map[string]relayshare.Demand
}

// record replaces the node's demand and forgets nodes that have not reported for a lease.
func (b *relayDemandBook) record(node string, demand map[string]relayshare.Demand, now time.Time) {
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

// demandFor is what each live relay reports for the account: node -> demand (only relays that report the account).
func (b *relayDemandBook) demandFor(account string, now time.Time) map[string]relayshare.Demand {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]relayshare.Demand{}
	for n, d := range b.byNode {
		if demand, ok := d.account[account]; ok && now.Sub(d.at) < relayNodeLease {
			out[n] = demand
		}
	}
	return out
}
