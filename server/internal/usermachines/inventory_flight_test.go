package usermachines

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A computation that blocks until its own gate opens and answers its call number as the total.
type gatedInventory struct {
	calls atomic.Int32
	gates [8]chan struct{}
}

func newGatedInventory() *gatedInventory {
	g := &gatedInventory{}
	for i := range g.gates {
		g.gates[i] = make(chan struct{})
	}
	return g
}

func (g *gatedInventory) compute(context.Context) ([]UserMachine, int, error) {
	n := int(g.calls.Add(1))
	<-g.gates[n-1]
	return []UserMachine{{ID: "machine_1"}}, n, nil
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) get() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

var flightKey = inventoryKey{account: "user_1", opts: InventoryOptions{Limit: 50}.key()}

// 265 daemons of one account listed it at once after one change (19 concurrent 2-5 s lists on production). A burst
// of identical requests costs one computation, and every caller gets the list.
func TestIdenticalInventoryRequestsShareComputations(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	flights := inventoryFlights{now: clock.get}
	g := newGatedInventory()
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, _, err := flights.do(context.Background(), flightKey, g.compute)
			if err == nil && (len(items) != 1 || items[0].ID != "machine_1") {
				err = errors.New("wrong items")
			}
			errs <- err
		}()
	}
	waitFor(t, "the computation", func() bool { return g.calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	close(g.gates[0])
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("50 identical requests within the share window ran %d computations, want 1", n)
	}
}

// Staging (50 daemons of one account, steady load): queueing every later request behind the next computation
// serialized the account's lists and timed them out at 15 s. A request joins only a computation that started at most
// inventoryShareWindow before it; a later one starts its own and does not wait for the running one.
func TestLaterInventoryRequestDoesNotQueueBehindARunningComputation(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	flights := inventoryFlights{now: clock.get}
	g := newGatedInventory()
	first, joined, later := make(chan int, 1), make(chan int, 1), make(chan int, 1)
	go func() { _, n, _ := flights.do(context.Background(), flightKey, g.compute); first <- n }()
	waitFor(t, "the first computation", func() bool { return g.calls.Load() == 1 })
	clock.add(inventoryShareWindow / 2)
	go func() { _, n, _ := flights.do(context.Background(), flightKey, g.compute); joined <- n }()
	time.Sleep(20 * time.Millisecond) // it joins inside the window
	clock.add(inventoryShareWindow)
	go func() { _, n, _ := flights.do(context.Background(), flightKey, g.compute); later <- n }()
	waitFor(t, "the second computation", func() bool { return g.calls.Load() == 2 })
	close(g.gates[1]) // the later request's own computation finishes while the first still runs
	select {
	case n := <-later:
		if n != 2 {
			t.Fatalf("later request got computation %d, want its own (2)", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a later request waited behind a running computation")
	}
	close(g.gates[0])
	if a, b := <-first, <-joined; a != 1 || b != 1 {
		t.Fatalf("first %d, joined %d; want both computation 1", a, b)
	}
	other := inventoryKey{account: "user_2", opts: flightKey.opts}
	close(g.gates[2])
	if _, n, _ := flights.do(context.Background(), other, g.compute); n != 3 {
		t.Fatalf("another account shared a computation: %d", n)
	}
}

// A caller that gives up gets its own error at once; the computation still answers the others and is forgotten.
func TestInventoryCallerCancellationDoesNotStopTheSharedComputation(t *testing.T) {
	var flights inventoryFlights
	g := newGatedInventory()
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() { _, _, err := flights.do(ctx, flightKey, g.compute); gaveUp <- err }()
	waitFor(t, "the computation", func() bool { return g.calls.Load() == 1 })
	stayed := make(chan int, 1)
	go func() { _, n, _ := flights.do(context.Background(), flightKey, g.compute); stayed <- n }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v", err)
	}
	close(g.gates[0])
	if got := <-stayed; got != 1 {
		t.Fatalf("waiting caller got computation %d, want the shared one", got)
	}
	waitFor(t, "the key to be released", func() bool {
		flights.mu.Lock()
		defer flights.mu.Unlock()
		return len(flights.keys) == 0
	})
}
