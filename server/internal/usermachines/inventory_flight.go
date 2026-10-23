package usermachines

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// One inventory change wakes the daemon of every machine in the account, and each lists the account's machines:
// an account with N machines asked for the same list N times at once (19 concurrent 2-5 s lists for a 265-machine
// account on production). Identical requests share a computation that started at most inventoryShareWindow before
// them, and at most inventoryMaxRunning computations of one key run at once: past that a request joins the newest
// running one. A burst costs one computation, and one account's lists can never hold more than a few database
// connections (unbounded, 50 daemons of one account held all 20 pool connections and every other request on the
// server waited for one at authentication). Queueing behind the *next* computation is what the first version did,
// and it serialized an account's lists into 15 s timeouts; a request here waits for at most one computation.
const (
	inventoryShareWindow = time.Second
	inventoryMaxRunning  = 2
)

type inventoryFlights struct {
	mu   sync.Mutex
	keys map[inventoryKey][]*inventoryFlight // the running computations of each key, oldest first
	now  func() time.Time                    // nil: time.Now
}

type inventoryKey struct {
	account string
	scoped  bool
	scope   workspace.Scope
	opts    string
}

type inventoryFlight struct {
	started time.Time
	done    chan struct{}
	items   []UserMachine
	total   int
	err     error
}

// inventoryComputeTimeout bounds a shared computation, which no single caller's cancellation stops.
const inventoryComputeTimeout = 30 * time.Second

func (o InventoryOptions) key() string {
	closed := "-"
	if o.Closed != nil {
		closed = fmt.Sprint(*o.Closed)
	}
	return fmt.Sprintf("%d|%d|%q|%q|%q|%s|%t|%q", o.Limit, o.Offset, o.Q, o.State, o.Owner, closed, o.WithoutLiveMetadata, o.MachineID)
}

func (f *inventoryFlights) do(ctx context.Context, key inventoryKey, compute func(context.Context) ([]UserMachine, int, error)) ([]UserMachine, int, error) {
	now := time.Now
	if f.now != nil {
		now = f.now
	}
	f.mu.Lock()
	if f.keys == nil {
		f.keys = map[inventoryKey][]*inventoryFlight{}
	}
	running := f.keys[key]
	var flight *inventoryFlight
	if len(running) > 0 {
		flight = running[len(running)-1]
	}
	if flight == nil || (now().Sub(flight.started) > inventoryShareWindow && len(running) < inventoryMaxRunning) {
		flight = &inventoryFlight{started: now(), done: make(chan struct{})}
		f.keys[key] = append(running, flight)
		// The values of this request (workspace selection) without its cancellation; bounded on its own.
		computeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inventoryComputeTimeout)
		go func() {
			defer cancel()
			flight.items, flight.total, flight.err = compute(computeCtx)
			close(flight.done)
			f.mu.Lock()
			rest := f.keys[key][:0]
			for _, other := range f.keys[key] {
				if other != flight {
					rest = append(rest, other)
				}
			}
			if len(rest) == 0 {
				delete(f.keys, key)
			} else {
				f.keys[key] = rest
			}
			f.mu.Unlock()
		}()
	}
	f.mu.Unlock()
	select {
	case <-flight.done:
		if flight.err != nil {
			return nil, 0, flight.err
		}
		return append(make([]UserMachine, 0, len(flight.items)), flight.items...), flight.total, nil
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}
