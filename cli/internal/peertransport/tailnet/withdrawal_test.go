package tailnet

import (
	"testing"
	"time"
)

// F5-10, close then cut: a withdrawn peer is refused new streams at once, keeps its packet path while its open
// sessions close (so their closes reach it), and loses the path when they have closed or the grace passed.
func TestWithdrawnPeerIsRefusedAtOnceAndCutAfterItsSessionsCloseOrTheGrace(t *testing.T) {
	previous := withdrawalGrace
	withdrawalGrace = 300 * time.Millisecond
	t.Cleanup(func() { withdrawalGrace = previous })
	a, cfg, signer, _ := networkTestAuthority(t)
	address := cfg.Peers[0].Identity.VirtualAddress
	installed := func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, admitted := a.peersLocked()
		for addr := range admitted {
			if addr.String() == address {
				return true
			}
		}
		return false
	}
	apply := func(generation uint64, withPeer bool) {
		t.Helper()
		next := cfg
		next.Generation = generation
		if !withPeer {
			next.Peers = nil
		}
		if err := a.Apply(t.Context(), networkToken(t, signer, next)); err != nil {
			t.Fatal(err)
		}
	}
	hold := func() func() {
		t.Helper()
		release, err := a.TrackAdmittedScope("machine_test", "grant_test", "terminal", "dial")
		if err != nil {
			t.Fatal(err)
		}
		return release
	}

	// Withdrawn while a session is open: refused at once, path kept, cut by the grace.
	apply(1, true)
	release := hold()
	apply(2, false)
	if a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("a withdrawn peer is still allowed")
	}
	if _, err := a.TrackAdmittedScope("machine_test", "grant_test", "terminal", "dial"); err == nil {
		t.Fatal("a new stream was admitted during the withdrawal grace")
	}
	if !installed() {
		t.Fatal("the withdrawn peer's path was cut before its open session could close")
	}
	time.Sleep(withdrawalGrace + 200*time.Millisecond)
	if installed() {
		t.Fatal("the withdrawn peer's path outlived the grace")
	}
	release()

	// Its last session closes within the grace: cut at once.
	apply(3, true)
	release = hold()
	apply(4, false)
	if !installed() {
		t.Fatal("path cut before the session closed")
	}
	release()
	if installed() {
		t.Fatal("the path stayed after the withdrawn peer's last session closed")
	}

	// Nothing open: cut with the withdrawal.
	apply(5, true)
	apply(6, false)
	if installed() {
		t.Fatal("an idle withdrawn peer kept its path")
	}
}
