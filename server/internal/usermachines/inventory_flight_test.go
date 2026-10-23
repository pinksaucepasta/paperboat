package usermachines

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A gated computation: each call blocks until released and answers its call number as the total.
type gatedInventory struct {
	calls   atomic.Int32
	release chan struct{}
}

func (g *gatedInventory) compute(context.Context) ([]UserMachine, int, error) {
	n := int(g.calls.Add(1))
	<-g.release
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

// 265 daemons of one account listed it at once after one change (19 concurrent 2-5 s lists on production). A burst
// of identical requests costs at most two computations, and every caller gets the list.
func TestIdenticalInventoryRequestsShareComputations(t *testing.T) {
	var flights inventoryFlights
	g := &gatedInventory{release: make(chan struct{})}
	key := inventoryKey{account: "user_1", opts: InventoryOptions{Limit: 50}.key()}
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, _, err := flights.do(context.Background(), key, g.compute)
			if err == nil && (len(items) != 1 || items[0].ID != "machine_1") {
				err = errors.New("wrong items")
			}
			errs <- err
		}()
	}
	waitFor(t, "the first computation", func() bool { return g.calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond) // let the burst queue behind it
	close(g.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := g.calls.Load(); n > 2 {
		t.Fatalf("50 identical requests ran %d computations, want at most 2", n)
	}
}

// Sharing must not serve stale data: a request that arrives while a list is computing waits for the next
// computation, which starts after it arrived, never the one already running.
func TestInventoryRequestNeverGetsAComputationStartedBeforeIt(t *testing.T) {
	var flights inventoryFlights
	g := &gatedInventory{release: make(chan struct{})}
	key := inventoryKey{account: "user_1", opts: InventoryOptions{Limit: 50}.key()}
	first, second := make(chan int, 1), make(chan int, 1)
	go func() { _, n, _ := flights.do(context.Background(), key, g.compute); first <- n }()
	waitFor(t, "the first computation", func() bool { return g.calls.Load() == 1 })
	go func() { _, n, _ := flights.do(context.Background(), key, g.compute); second <- n }()
	time.Sleep(20 * time.Millisecond)
	close(g.release)
	if got := <-first; got != 1 {
		t.Fatalf("first request got computation %d, want 1", got)
	}
	if got := <-second; got != 2 {
		t.Fatalf("a request that arrived during computation 1 got computation %d, want 2 (started after it)", got)
	}
	other := inventoryKey{account: "user_2", opts: key.opts}
	if _, n, _ := flights.do(context.Background(), other, g.compute); n != 3 {
		t.Fatalf("another account shared a computation: %d", n)
	}
}

// A caller that gives up gets its own error at once; the shared computation still answers the others.
func TestInventoryCallerCancellationDoesNotStopTheSharedComputation(t *testing.T) {
	var flights inventoryFlights
	g := &gatedInventory{release: make(chan struct{})}
	key := inventoryKey{account: "user_1", opts: InventoryOptions{Limit: 50}.key()}
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() { _, _, err := flights.do(ctx, key, g.compute); gaveUp <- err }()
	waitFor(t, "the computation", func() bool { return g.calls.Load() == 1 })
	stayed := make(chan int, 1)
	go func() { _, n, _ := flights.do(context.Background(), key, g.compute); stayed <- n }()
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v", err)
	}
	close(g.release)
	if got := <-stayed; got != 2 {
		t.Fatalf("waiting caller got computation %d, want 2", got)
	}
	waitFor(t, "the key to be released", func() bool {
		flights.mu.Lock()
		defer flights.mu.Unlock()
		return len(flights.keys) == 0
	})
}
