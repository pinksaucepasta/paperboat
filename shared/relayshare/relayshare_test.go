package relayshare

import (
	"math"
	"testing"
)

// An account's speed divided among its relays by demand: the shares add up to the speed, an idle relay holds the floor (5% of the
// speed) and a lone busy one gets at least 90% of it, so one flow on one of two relays runs at nearly the plan speed.
func TestShareDividesTheAccountSpeedByDemand(t *testing.T) {
	cases := []struct {
		name   string
		mbit   float64
		demand map[string]float64
	}{
		{"one busy of two", 10, map[string]float64{"a": 1e6, "b": 0}},
		{"two equal", 10, map[string]float64{"a": 1e6, "b": 1e6}},
		{"three, uneven", 100, map[string]float64{"a": 3e6, "b": 1e6, "c": 5e4}},
		{"no demand", 10, map[string]float64{"a": 0, "b": 0}},
		{"slow tier", 0.512, map[string]float64{"a": 1e4, "b": 0}},
	}
	for _, c := range cases {
		var total float64
		for _, d := range c.demand {
			total += Share(c.mbit, d, c.demand)
		}
		if math.Abs(total-c.mbit) > 1e-9 {
			t.Errorf("%s: shares add up to %v, want %v", c.name, total, c.mbit)
		}
	}
	busy := map[string]float64{"a": 1e6, "b": 0}
	if got := Share(10, 1e6, busy); got < 9 || got != 9.5 {
		t.Errorf("a lone busy relay of two got %v of 10, want 9.5 (at least 90%%)", got)
	}
	if got := Share(10, 0, busy); got != 0.5 {
		t.Errorf("the idle relay got %v, want its floor 0.5", got)
	}
	if got := Share(10, 5, map[string]float64{"a": 5}); got != 10 {
		t.Errorf("a lone relay got %v, want the whole speed", got)
	}
	if got := Share(10, 0, map[string]float64{"a": 0, "b": 0}); got != 5 {
		t.Errorf("no demand anywhere gave %v, want an even split 5", got)
	}
}
