// Package relayshare divides an account's relay speed among the relays that carry it, by what each reports. The control plane computes
// the shares; the relay's tests use the same function.
package relayshare

import "github.com/pinksaucepasta/paperboat/shared/relaylimits"

// Demand is what an account offered one relay over a window, in bytes per second: Offered is everything that arrived, Held the part the
// relay's speed limit delayed or dropped. A relay that held traffic back has a flow that wants more than it gets.
type Demand struct {
	Offered float64 `json:"offered"`
	Held    float64 `json:"held"`
}

// limited reports whether the relay held back a real part of the account's traffic (a few delayed packets of a flow that fits its share
// do not count).
func (d Demand) limited() bool { return d.Held > 0 && d.Held >= 0.1*d.Offered }

// Shares divides an account's speed mbit among the relays that report demand (relay id -> demand), summing to mbit: max-min fairness.
// A relay whose flows fit (nothing held back) keeps what it offers plus 25% (at least relaylimits.AccountStartMbit); the relays that are held back share the
// rest equally, so one busy relay gets everything but its idle neighbours' start allowance, and two busy relays get half each at once.
// With nobody held back the spare speed is spread evenly.
// Prior art: max-min fair allocation as in Google Doorman's FairShare (github.com/youtube/doorman, doc/algorithms.md), where each
// client keeps what it asks for up to the fair share and the rest is split among the clients that want more.
func Shares(mbit float64, demand map[string]Demand) map[string]float64 {
	n := len(demand)
	out := make(map[string]float64, n)
	if n == 0 {
		return out
	}
	if n == 1 {
		for id := range demand {
			out[id] = mbit
		}
		return out
	}
	start := min(relaylimits.AccountStartMbit, mbit/float64(n))
	var wants float64 // what the relays that are not held back keep
	var held int
	for id, d := range demand {
		if d.limited() {
			held++
			continue
		}
		want := max(start, 1.25*d.Offered*8/1e6)
		out[id] = want
		wants += want
	}
	// The relays held back each keep at least the start allowance; the rest of the speed is what the others may take.
	if available := mbit - float64(held)*start; wants > available {
		for id := range out {
			out[id] *= available / wants
		}
		wants = available
	}
	switch {
	case held > 0:
		for id, d := range demand {
			if d.limited() {
				out[id] = (mbit - wants) / float64(held)
			}
		}
	default:
		spare := (mbit - wants) / float64(n)
		for id := range out {
			out[id] += spare
		}
	}
	return out
}
