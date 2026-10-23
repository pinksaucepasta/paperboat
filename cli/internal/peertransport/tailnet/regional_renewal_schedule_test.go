package tailnet

import (
	"testing"
	"time"
)

// Relays went unusable for ~2 s every ~20 s on hp: recovery renewed 9.2-9.8 s after the last
// renewal, but the signed observations it got were already up to 5 s old and end 15 s after
// they were made, so the next renewal often came after they had ended.
func TestRegionalRenewalFollowsTheEarliestObservation(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	at := func(health, capacity int64) RegionalNode {
		return RegionalNode{ObservedAt: now.Unix() - health, CapacityObservedAt: now.Unix() - capacity}
	}
	for _, c := range []struct {
		name  string
		nodes []RegionalNode
		want  time.Duration
	}{
		{"fresh at issuance", []RegionalNode{at(0, 0)}, 12 * time.Second},
		{"five seconds old", []RegionalNode{at(5, 5)}, 7 * time.Second},
		{"earliest node decides", []RegionalNode{at(1, 1), at(6, 2)}, 6 * time.Second},
		{"older capacity observation decides", []RegionalNode{at(2, 8)}, 4 * time.Second},
		{"already old stays bounded", []RegionalNode{at(13, 13)}, regionalMinimumRenewal},
	} {
		if got := regionalRenewalAfter(c.nodes, now); got != c.want {
			t.Errorf("%s: renew after %v, want %v", c.name, got, c.want)
		}
	}
	if _, ok := (&Authority{}).regionalRenewalDelay(now); ok {
		t.Error("an authority without eligible relays scheduled an observation renewal")
	}
}
