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
// account on production, where one list takes 20 ms unloaded). Identical requests share a computation, but only
// one that starts after they arrived, so every answer reflects a snapshot taken after its request: a request
// that finds a list already running waits for the next one, which every request arriving meanwhile shares. A
// burst costs at most two computations per key.
type inventoryFlights struct {
	mu   sync.Mutex
	keys map[inventoryKey]*inventoryFlightPair
}

type inventoryKey struct {
	account string
	scoped  bool
	scope   workspace.Scope
	opts    string
}

type inventoryFlightPair struct {
	running, next *inventoryFlight
}

type inventoryFlight struct {
	ctx   context.Context // the first waiter's values (workspace), not its cancellation
	done  chan struct{}
	items []UserMachine
	total int
	err   error
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
	f.mu.Lock()
	if f.keys == nil {
		f.keys = map[inventoryKey]*inventoryFlightPair{}
	}
	pair := f.keys[key]
	var flight *inventoryFlight
	switch {
	case pair == nil:
		flight = &inventoryFlight{ctx: ctx, done: make(chan struct{})}
		f.keys[key] = &inventoryFlightPair{running: flight}
		go f.run(key, flight, compute)
	case pair.next == nil:
		flight = &inventoryFlight{ctx: ctx, done: make(chan struct{})}
		pair.next = flight
	default:
		flight = pair.next
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

// run computes flight, then starts the flight queued behind it, if any.
func (f *inventoryFlights) run(key inventoryKey, flight *inventoryFlight, compute func(context.Context) ([]UserMachine, int, error)) {
	for flight != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(flight.ctx), inventoryComputeTimeout)
		flight.items, flight.total, flight.err = compute(ctx)
		cancel()
		close(flight.done)
		f.mu.Lock()
		pair := f.keys[key]
		flight, pair.running, pair.next = pair.next, pair.next, nil
		if flight == nil {
			delete(f.keys, key)
		}
		f.mu.Unlock()
	}
}
