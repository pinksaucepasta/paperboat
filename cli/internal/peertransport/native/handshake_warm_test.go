package native

import (
	"context"
	"testing"
	"time"
)

// Setups to one peer take turns only until one dial has got through; then they run side by side. Taking turns
// after that cost one QUIC round trip per exec: 32 concurrent execs over a 260 ms relay finished up to 8 s late
// (F21-166). A dial that fails makes the peer cold again.
func TestHandshakeTurnsEndOnceADialGotThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o := &Owner{ctx: ctx}
	acquired := func(peer string) chan func() {
		got := make(chan func(), 1)
		go func() {
			if release, err := o.acquireHandshake(ctx, peer); err == nil {
				got <- release
			}
		}()
		return got
	}
	first := <-acquired("machine_a")
	second := acquired("machine_a")
	select {
	case <-second:
		t.Fatal("a second setup to a cold peer did not wait for the first")
	case <-time.After(100 * time.Millisecond):
	}
	other := <-acquired("machine_b") // another peer is never held up
	other()
	o.markHandshakeWarm("machine_a", true) // the first dial got through
	var release2 func()
	select {
	case release2 = <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued setup stayed behind a dial that had got through")
	}
	select {
	case release3 := <-acquired("machine_a"): // a setup arriving later runs at once too
		release3()
	case <-time.After(5 * time.Second):
		t.Fatal("a setup to a warm peer waited")
	}
	o.markHandshakeWarm("machine_a", false) // a dial failed: cold again
	first()
	release2()
	third := <-acquired("machine_a") // idle: the token is free again
	fourth := acquired("machine_a")
	select {
	case <-fourth:
		t.Fatal("a failed dial did not make the peer cold again")
	case <-time.After(100 * time.Millisecond):
	}
	third()
	(<-fourth)()
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.handshakes) != 0 {
		t.Fatalf("%d handshake slots left", len(o.handshakes))
	}
}
