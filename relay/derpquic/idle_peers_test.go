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
	settle := func() int {
		time.Sleep(300 * time.Millisecond)
		runtime.GC()
		return runtime.NumGoroutine()
	}
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
	deadline := time.Now().Add(1500 * time.Millisecond)
	s.Drain(deadline)
	select {
	case <-cc.conn.Context().Done():
		t.Fatal("connection closed before the drain deadline")
	case <-time.After(700 * time.Millisecond):
	}
	select {
	case <-cc.conn.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("connection still open after the drain deadline")
	}
	if time.Now().Before(deadline) {
		t.Fatal("closed before the deadline")
	}
	if err := classify(context.Cause(cc.conn.Context())); !errors.Is(err, ErrOverload) {
		t.Fatalf("drained connection: %v", err)
	}
}
