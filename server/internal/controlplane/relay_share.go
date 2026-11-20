package controlplane

import (
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"github.com/pinksaucepasta/paperboat/shared/relayshare"
)

// relayDemandBook holds, per relay node, what each connected account offered it and what the limit held back (bytes per second) in the
// node's last observation, and the share of the account's speed it was given then, in memory: demand changes every observation and is
// valid for one lease. A server restart leaves each relay alone for the few seconds until the others report, so it briefly gets the
// account's full speed.
type relayDemandBook struct {
	mu     sync.Mutex
	byNode map[string]relayNodeDemand
}

type relayNodeDemand struct {
	at      time.Time
	account map[string]relayshare.Demand
	share   map[string]float64 // Mbit/s the node was given for each account in the same observation
}

// divide records the node's demand and divides each planned account's speed (plan: account -> Mbit/s) among the relays that carry it
// by demand (relayshare.Shares); it returns the node's share for the accounts that more than one relay carries. Relays install their
// shares at different moments (each when it observes), so a share is never raised beyond what the other relays' last shares leave of
// the speed: the shares given out never add up to more than the speed (plus a start allowance), and a share moves from one relay to
// another over two observations instead of both holding it for one. Demand and shares of relays silent for a lease are forgotten.
// shortcut: a relay that stops observing but keeps forwarding (a partial outage) is forgotten after a lease while still applying its
// share, and a restarted server has no shares to hold back, so either briefly allows more than the speed; upgrade when real traffic
// shows it matters (the relay keeps its last speeds while control is away, by the offline-until-revoked rule).
func (b *relayDemandBook) divide(node string, demand map[string]relayshare.Demand, plan map[string]float64, now time.Time) map[string]float64 {
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
	entry := relayNodeDemand{at: now, account: demand, share: make(map[string]float64, len(plan))}
	b.byNode[node] = entry
	shares := map[string]float64{}
	for account, mbit := range plan {
		carried := map[string]relayshare.Demand{node: demand[account]} // this relay carries it even before it reports it
		var others float64
		for n, d := range b.byNode {
			if dm, ok := d.account[account]; ok && n != node {
				carried[n] = dm
				others += d.share[account]
			}
		}
		if len(carried) == 1 {
			entry.share[account] = mbit
			continue
		}
		share := min(relayshare.Shares(mbit, carried)[node], max(mbit-others, relaylimits.AccountStartMbit))
		entry.share[account] = share
		shares[account] = share
	}
	return shares
}
