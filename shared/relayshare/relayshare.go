// Package relayshare divides an account's relay speed among the relays that carry it, by the demand each reports. The control plane
// computes the shares; tests of the relay use the same function.
package relayshare

import "math"

// floorOf is the least share of an account's speed a relay that carries it gets, so a new flow on an idle relay can start and
// show its demand: 5% of the speed, and enough for control traffic.
func floorOf(mbit float64, relays int) float64 {
	return min(max(0.05*mbit, MinMbit), mbit/float64(relays))
}

// MinMbit keeps one relay's share of an account able to carry its control traffic.
const MinMbit = 0.064

// Share is one relay's share, in Mbit/s, of an account's speed mbit across relays that offered demand (bytes per second, the
// relay's own among them): the floor plus the rest in proportion to demand, so the shares of all the relays add up to the speed; an
// idle relay holds the floor, a lone busy one nearly everything, and with no demand anywhere the speed is split evenly.
// Prior art: Google Doorman's ProportionalShare (github.com/youtube/doorman, doc/algorithms.md): clients report what they want and
// the server divides the capacity by it.
func Share(mbit, own float64, demand map[string]float64) float64 {
	n := len(demand)
	if n <= 1 {
		return mbit
	}
	var sum float64
	for _, d := range demand {
		if d > 0 && !math.IsInf(d, 0) {
			sum += d
		}
	}
	if sum <= 0 || own < 0 {
		return mbit / float64(n)
	}
	floor := floorOf(mbit, n)
	return floor + (mbit-float64(n)*floor)*own/sum
}
