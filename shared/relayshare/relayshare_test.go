package relayshare

import (
	"math"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

const mbyte = 1e6 / 8 // bytes per second of one Mbit/s

func busy(mbit float64) Demand   { return Demand{Offered: 2 * mbit * mbyte, Held: mbit * mbyte} }
func steady(mbit float64) Demand { return Demand{Offered: mbit * mbyte} }

// An account's speed divided among its relays: the shares add up to the speed, one busy relay gets everything but its idle neighbours'
// start allowance (at least 95% of the speed: the user's one active flow runs at the plan speed), two busy relays get half each at
// once, and a relay whose flows fit keeps what it uses.
func TestSharesDivideTheAccountSpeedMaxMin(t *testing.T) {
	cases := []struct {
		name   string
		mbit   float64
		demand map[string]Demand
		want   map[string]float64
	}{
		{"one busy, one idle", 10, map[string]Demand{"a": busy(5), "b": {}}, map[string]float64{"a": 10 - relaylimits.AccountStartMbit, "b": relaylimits.AccountStartMbit}},
		{"two busy", 10, map[string]Demand{"a": busy(5), "b": busy(5)}, map[string]float64{"a": 5, "b": 5}},
		{"one fits, one busy", 10, map[string]Demand{"a": steady(2), "b": busy(5)}, map[string]float64{"a": 2.5, "b": 7.5}},
		{"nobody held back", 10, map[string]Demand{"a": {}, "b": {}}, map[string]float64{"a": 5, "b": 5}},
		{"three, two busy", 100, map[string]Demand{"a": busy(40), "b": busy(40), "c": {}}, map[string]float64{"a": 49.968, "b": 49.968, "c": relaylimits.AccountStartMbit}},
		{"slow tier, one busy", 0.512, map[string]Demand{"a": busy(0.2), "b": {}}, map[string]float64{"a": 0.512 - relaylimits.AccountStartMbit, "b": relaylimits.AccountStartMbit}},
		{"one relay", 10, map[string]Demand{"a": busy(5)}, map[string]float64{"a": 10}},
	}
	for _, c := range cases {
		got := Shares(c.mbit, c.demand)
		var total float64
		for id, want := range c.want {
			total += got[id]
			if math.Abs(got[id]-want) > 1e-9 {
				t.Errorf("%s: relay %s got %v, want %v", c.name, id, got[id], want)
			}
		}
		if math.Abs(total-c.mbit) > 1e-9 {
			t.Errorf("%s: shares add up to %v, want %v", c.name, total, c.mbit)
		}
	}
	if got := Shares(10, map[string]Demand{"a": busy(5), "b": {}})["a"]; got < 9.5 {
		t.Errorf("one active flow got %v of 10, want at least 95%%", got)
	}
	// Demand that does not fit the speed is scaled down: the shares still add up to it.
	crowded := Shares(10, map[string]Demand{"a": steady(9), "b": steady(9), "c": busy(1)})
	if total := crowded["a"] + crowded["b"] + crowded["c"]; math.Abs(total-10) > 1e-9 || crowded["c"] < relaylimits.AccountStartMbit-1e-9 {
		t.Errorf("crowded shares %v, want a sum of 10 with the held relay at least its start allowance", crowded)
	}
}
