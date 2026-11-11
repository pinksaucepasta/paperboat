package mesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"tailscale.com/tailcfg"
)

// Review of b6eb2018c, P2: one failed connect held a region for 5 s, so a relay back 200 ms later got nothing for
// another 4.8 s. A failure now holds the region for 250 ms, doubling with each further failure up to 5 s, and a
// success resets the backoff, so a relay that comes back is used again within one step.
func TestRelayComesBackWithinOneBackoffStep(t *testing.T) {
	var r relayConnects
	down := true
	dials := 0
	dial := func(context.Context, tailcfg.DERPRegionID) error {
		dials++
		if down {
			return errors.New("relay down")
		}
		return nil
	}
	const region = tailcfg.DERPRegionID(7)
	if err := r.ensure(t.Context(), region, dial); err == nil {
		t.Fatal("a down relay connected")
	}
	if err := r.ensure(t.Context(), region, dial); !errors.Is(err, errRelayConnectHeld) || dials != 1 {
		t.Fatalf("inside the first backoff step: err %v, dials %d", err, dials)
	}
	down = false // the relay is back 0 ms after the failure
	time.Sleep(relayConnectBackoff + 20*time.Millisecond)
	if err := r.ensure(t.Context(), region, dial); err != nil || dials != 2 {
		t.Fatalf("one step after the failure: err %v, dials %d", err, dials)
	}
	for fails, want := range map[int]time.Duration{1: 250 * time.Millisecond, 2: 500 * time.Millisecond, 4: 2 * time.Second, 5: 4 * time.Second, 6: 5 * time.Second, 40: 5 * time.Second} {
		if got := relayConnectBackoffAfter(fails); got != want {
			t.Errorf("backoff after %d failures = %v, want %v", fails, got, want)
		}
	}
	if r.fails[region] != 0 {
		t.Fatalf("a success left %d failures", r.fails[region])
	}
}
