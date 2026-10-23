package peersessions

import (
	"testing"
)

func TestBoundGrantsKeepsTheFirstEntriesInsteadOfFailing(t *testing.T) {
	few := make([]int, 10)
	if got := boundGrants(few); len(got) != 10 {
		t.Fatalf("a small set was changed: %d", len(got))
	}
	many := make([]int, 200)
	for i := range many {
		many[i] = i
	}
	got := boundGrants(many)
	if len(got) != maximumNetworkGrants || got[0] != 0 || got[len(got)-1] != maximumNetworkGrants-1 {
		t.Fatalf("bounded set = %d entries (first %d, last %d)", len(got), got[0], got[len(got)-1])
	}
}
