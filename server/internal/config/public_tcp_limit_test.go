package config

import "testing"

// Public TCP tunnels need Pro (user decision 2026-11-28): Free defaults to 0 and 0 is valid; Pro must allow at least one.
func TestPublicTCPLimitFreeIsZero(t *testing.T) {
	c := Default()
	if got := c.Billing.Limits.PublicTCP; got.Free != 0 || got.Pro != 10 || got.TeamPerSeat != 10 {
		t.Fatalf("default public TCP limits %+v, want Free 0, Pro 10, Team 10 per seat", got)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Free 0 refused: %v", err)
	}
	for name, mutate := range map[string]func(*PublicTCPLimit){
		"negative free":  func(l *PublicTCPLimit) { l.Free = -1 },
		"zero pro":       func(l *PublicTCPLimit) { l.Pro = 0 },
		"pro below free": func(l *PublicTCPLimit) { l.Free, l.Pro = 5, 2 },
		"zero team":      func(l *PublicTCPLimit) { l.TeamPerSeat = 0 },
	} {
		bad := Default()
		mutate(&bad.Billing.Limits.PublicTCP)
		if bad.Validate() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
