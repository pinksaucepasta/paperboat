package derpquic

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// F21-34 (R3): an admitted idle connection costs no periodic work of its own. A per-connection 1 s poll took the
// relay's global lock once per second per connection (about 16k takes a second at 16k leases); revocation,
// supersede and drain now close connections where they happen. The proxy measured here is goroutines per
// admitted idle connection: a poller adds one per connection and fails the budget.
func TestAdmittedIdlePeersRunNoPerConnectionPoller(t *testing.T) {
	const peers = 40
	f := newFixture(t)
	_, address, _ := f.start("127.0.0.1:0")
	settle := settledGoroutines
	before := settle()
	for i := range peers {
		cert := certificate(t)
		connect(t, f.client(address, cert, f.grant(key.NewNode().Public(), cert, string(rune('a'+i%26))+string(rune('a'+i/26)))))
	}
	perPeer := float64(settle()-before) / peers
	t.Logf("%.2f goroutines per admitted idle connection", perPeer)
	if perPeer > baselineGoroutinesPerPeer {
		t.Fatalf("%.2f goroutines per admitted idle connection, budget %.2f: a per-connection poller came back", perPeer, baselineGoroutinesPerPeer)
	}
}

// settledGoroutines is the goroutine count once it has not moved for a full second of GC-and-look samples: the
// goroutines of connections still handshaking or of an earlier test still winding down are given as long as they need
// instead of a fixed sleep, and a count that is still moving is never reported.
func settledGoroutines() int {
	const sample, steady = 50 * time.Millisecond, 20
	deadline := time.Now().Add(waitBound)
	last, same := -1, 0
	for time.Now().Before(deadline) {
		runtime.GC()
		if n := runtime.NumGoroutine(); n == last {
			if same++; same >= steady {
				return n
			}
		} else {
			last, same = n, 0
		}
		time.Sleep(sample)
	}
	return runtime.NumGoroutine()
}

// A count taken while goroutines are still exiting would blame them on the peers: the goroutines here exit half a
// second in, a quarter of the way through the settling window.
func TestSettledGoroutinesWaitsForExitingGoroutines(t *testing.T) {
	base := settledGoroutines()
	release := make(chan struct{})
	for range 20 {
		go func() { <-release }()
	}
	time.AfterFunc(500*time.Millisecond, func() { close(release) })
	if got := settledGoroutines(); got > base+2 {
		t.Fatalf("settled at %d goroutines with 20 still exiting, base %d", got, base)
	}
}

// 13.00 measured; the per-connection poller made it 14.00 (client and server share the test process).
const baselineGoroutinesPerPeer = 13.3

// A drain deadline in the future leaves admitted connections up until it passes, then one timer ends them all
// with the overload code.
func TestDrainClosesAdmittedConnectionsAtTheDeadlineNotBefore(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	cert := certificate(t)
	c := f.client(address, cert, f.grant(key.NewNode().Public(), cert, "a"))
	connect(t, c)
	c.mu.Lock()
	cc := c.current
	c.mu.Unlock()
	// The close is timestamped where it happens, so an observer that stalls past the deadline, as one on a loaded
	// runner can, still catches a close that came before it.
	closed := make(chan time.Time, 1)
	context.AfterFunc(cc.conn.Context(), func() { closed <- time.Now() })
	deadline := time.Now().Add(400 * time.Millisecond)
	s.Drain(deadline)
	time.Sleep(800 * time.Millisecond)
	select {
	case at := <-closed:
		if at.Before(deadline) {
			t.Fatalf("closed %v before the deadline", deadline.Sub(at))
		}
	case <-time.After(waitBound):
		t.Fatal("connection still open after the drain deadline")
	}
	if err := classify(context.Cause(cc.conn.Context())); !errors.Is(err, ErrOverload) {
		t.Fatalf("drained connection: %v", err)
	}
}
