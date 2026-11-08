package native

import (
	"testing"
	"time"
)

// Review of d9c5c62db/66df74a4f: the window opened when the mark was consumed, but the handshake starts only after
// PrepareRegional (up to 5 s), the carrier dial and lease waits, so on a cold relay it expired first; it was a hard
// 3 s although the commit said "until one succeeds"; and marks and measurements never expired.
func TestRestartTrackerWindowStartsAtTheFirstHandshakeAndLastsUntilOneSucceeds(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var r restartTracker
	r.succeeded("p", 47*time.Millisecond, t0)
	r.markClosed("p", t0)
	if !r.consume("p", t0) {
		t.Fatal("a fresh mark was not consumed")
	}
	// A cold relay: PrepareRegional and the lease took 6 s before the first handshake. The window starts now.
	if got := r.wait("p", false, t0.Add(6*time.Second)); got != restartHandshake(47*time.Millisecond) {
		t.Fatalf("first handshake after the mark got %v, want the short wait", got)
	}
	r.failed("p", restartHandshake(47*time.Millisecond), t0.Add(8800*time.Millisecond)) // a short handshake failed 2.8 s in: the window moves out
	if got := r.wait("p", false, t0.Add(11*time.Second)); got == 0 {
		t.Fatal("a failed short handshake did not extend the window")
	}
	// Failures cannot extend it past restartQuickMax from its start.
	for at := 12 * time.Second; at < 30*time.Second; at += 2 * time.Second {
		r.failed("p", restartHandshake(47*time.Millisecond), t0.Add(at))
	}
	if got := r.wait("p", false, t0.Add(6*time.Second+restartQuickMax+time.Millisecond)); got != 0 {
		t.Fatalf("window outlived restartQuickMax: %v", got)
	}
	// A new restart; one success ends the window at once.
	r.markClosed("p", t0.Add(time.Minute))
	r.consume("p", t0.Add(time.Minute))
	r.wait("p", false, t0.Add(time.Minute))
	r.succeeded("p", 50*time.Millisecond, t0.Add(time.Minute+time.Second))
	if got := r.wait("p", false, t0.Add(time.Minute+2*time.Second)); got != 0 {
		t.Fatalf("a successful handshake left the window open: %v", got)
	}
}

func TestRestartTrackerMarksAndMeasurementsExpire(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var r restartTracker
	r.markClosed("p", t0)
	if r.consume("p", t0.Add(restartMarkTTL)) {
		t.Fatal("a mark older than its TTL still counted as a restart")
	}
	r.markClosed("p", t0)
	if !r.consume("p", t0.Add(restartMarkTTL-time.Second)) {
		t.Fatal("a mark inside its TTL did not count")
	}
	// A measurement older than its TTL says nothing about the path now: a remote peer keeps the normal timeout,
	// a peer on this machine falls back to the floor.
	r.succeeded("far", 100*time.Millisecond, t0)
	r.markClosed("far", t0.Add(handshakeTookTTL+time.Minute))
	r.consume("far", t0.Add(handshakeTookTTL+time.Minute))
	if got := r.wait("far", false, t0.Add(handshakeTookTTL+time.Minute)); got != 0 {
		t.Fatalf("a stale measurement set a short wait for a remote peer: %v", got)
	}
	if got := r.wait("here", true, t0); got != peerRestartHandshake {
		t.Fatalf("a local peer with no measurement got %v, want %v", got, peerRestartHandshake)
	}
}

// A peer on this machine with no measurement got the 300 ms floor forever: a timed-out handshake recorded nothing, so
// on a loaded machine whose local handshake needs more, every dial failed at 300 ms. A timeout now counts as "took at
// least the wait": the next dial waits three times longer, then the normal timeout; a success restores the fast path.
func TestRestartTrackerLocalPeerSlowerThanTheFloorReachesTheNormalTimeout(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var r restartTracker
	wait := r.wait("here", true, t0)
	if wait != peerRestartHandshake {
		t.Fatalf("first local wait %v, want the floor", wait)
	}
	r.failed("here", wait, t0.Add(wait))
	if wait = r.wait("here", true, t0.Add(time.Second)); wait != 3*peerRestartHandshake {
		t.Fatalf("after a timed-out floor the wait is %v, want %v", wait, 3*peerRestartHandshake)
	}
	r.failed("here", wait, t0.Add(2*time.Second))
	if wait = r.wait("here", true, t0.Add(3*time.Second)); wait != 0 {
		t.Fatalf("after two timeouts the wait is %v, want the normal timeout", wait)
	}
	r.succeeded("here", 40*time.Millisecond, t0.Add(4*time.Second))
	if wait = r.wait("here", true, t0.Add(5*time.Second)); wait != peerRestartHandshake {
		t.Fatalf("a fast handshake left the wait at %v, want the floor", wait)
	}
	// A dial on the normal timeout that fails says nothing about the path: nothing is recorded.
	r.failed("far", 0, t0)
	if _, recorded := r.took["far"]; recorded || r.wait("far", false, t0) != 0 {
		t.Fatal("a normal-timeout failure was recorded as a measurement")
	}
}
