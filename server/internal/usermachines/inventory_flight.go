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
// them; any later request starts its own. A burst costs one computation, steady load at most one new computation
// per key per window, and no request waits for more than one computation. (Queueing every later request behind the
// next computation serialized an account's lists under steady load: 50 daemons of one account timed out at 15 s.)
const inventoryShareWindow = time.Second

type inventoryFlights struct {
	mu   sync.Mutex
	keys map[inventoryKey]*inventoryFlight // the newest computation of each key, while it runs
	now  func() time.Time                  // nil: time.Now
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
	return fmt.Sprintf("%d|%d|%q|%q|%q|%s|%t", o.Limit, o.Offset, o.Q, o.State, o.Owner, closed, o.WithoutLiveMetadata)
}

func (f *inventoryFlights) do(ctx context.Context, key inventoryKey, compute func(context.Context) ([]UserMachine, int, error)) ([]UserMachine, int, error) {
	now := time.Now
	if f.now != nil {
		now = f.now
	}
	f.mu.Lock()
	if f.keys == nil {
		f.keys = map[inventoryKey]*inventoryFlight{}
	}
	flight := f.keys[key]
	if flight == nil || now().Sub(flight.started) > inventoryShareWindow {
		flight = &inventoryFlight{started: now(), done: make(chan struct{})}
		f.keys[key] = flight
		// The values of this request (workspace selection) without its cancellation; bounded on its own.
		computeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inventoryComputeTimeout)
		go func() {
			defer cancel()
			flight.items, flight.total, flight.err = compute(computeCtx)
			close(flight.done)
			f.mu.Lock()
			if f.keys[key] == flight {
				delete(f.keys, key)
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
		return append([]UserMachine(nil), flight.items...), flight.total, nil
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}
