package controlplane

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"github.com/pinksaucepasta/paperboat/shared/relayshare"
)

// Relays install their shares when they observe, at different moments: a share never rises beyond what the other relays' last shares
// leave, so the shares given out stay within the speed (plus a start allowance) and a share moves from one relay to the other over two
// observations; a relay silent for a lease is forgotten.
func TestRelayDemandBookNeverGivesOutMoreThanTheSpeed(t *testing.T) {
	const speed = 10.0
	var book relayDemandBook
	now := time.Now()
	plan := map[string]float64{"acct": speed}
	busy := map[string]relayshare.Demand{"acct": {Offered: 2e6, Held: 1e6}}
	idle := map[string]relayshare.Demand{"acct": {}}
	given := map[string]float64{}
	observe := func(node string, demand map[string]relayshare.Demand) {
		t.Helper()
		now = now.Add(time.Second)
		share, shared := book.divide(node, demand, plan, now)["acct"]
		if !shared {
			share = speed
		}
		given[node] = share
		if sum := given["a"] + given["b"]; sum > speed+relaylimits.AccountStartMbit+1e-9 {
			t.Fatalf("after %s observed, the shares given out add up to %.3f of a %.0f Mbit/s speed (a %.3f, b %.3f)", node, sum, speed, given["a"], given["b"])
		}
	}
	observe("a", busy) // alone: the whole speed
	observe("b", busy) // wants half, but a still holds everything
	if given["b"] != relaylimits.AccountStartMbit {
		t.Fatalf("b was given %.3f while a held the speed, want the start allowance", given["b"])
	}
	observe("a", busy) // a gives up its half
	observe("b", busy) // and b takes it
	if given["a"] != 5 || given["b"] != 5 {
		t.Fatalf("after two observations each: a %.3f, b %.3f, want 5 and 5", given["a"], given["b"])
	}
	observe("b", idle) // b has nothing to send: a may take the speed back
	observe("a", busy)
	if given["a"] < speed-relaylimits.AccountStartMbit-1e-9 {
		t.Fatalf("a was given %.3f once b was idle, want all but b's start allowance", given["a"])
	}
	now = now.Add(relayNodeLease + time.Second) // both silent: forgotten, the next to observe is alone
	if _, shared := book.divide("b", busy, plan, now)["acct"]; shared {
		t.Fatal("a relay silent for a lease still counts as a carrier")
	}
}
