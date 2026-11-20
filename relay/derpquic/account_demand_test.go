package derpquic

import (
	"testing"
	"time"
)

// An account's demand is what it offered: bytes the limit let through count once, bytes it delayed or dropped count twice (the flow
// wanted more than it got), so a flow held at a small share still shows it can use more. takeOffered starts a new window.
func TestAccountBudgetCountsDemandTwiceForHeldBackBytes(t *testing.T) {
	b := newAccountBudget(1000) // 1000 B/s, burst at least two fragments
	now := time.Unix(1000, 0)
	if _, ok := b.reserve(now, 500, 0); !ok {
		t.Fatal("within the burst")
	}
	if got := b.takeOffered(); got != 500 {
		t.Fatalf("a packet within the limit offered %d, want 500", got)
	}
	for b.tokens > 0 { // use up the burst
		b.reserve(now, 500, time.Hour)
	}
	b.takeOffered()
	if delay, ok := b.reserve(now, 1000, time.Hour); !ok || delay <= 0 {
		t.Fatalf("over the limit: delay %v ok %v, want a delay", delay, ok)
	}
	if got := b.takeOffered(); got != 2000 {
		t.Fatalf("a delayed packet offered %d, want 1000 counted twice", got)
	}
	if _, ok := b.reserve(now, 1000, 0); ok {
		t.Fatal("a wait past the maximum is dropped")
	}
	if got := b.takeOffered(); got != 2000 {
		t.Fatalf("a dropped packet offered %d, want 1000 counted twice", got)
	}
	if got := b.takeOffered(); got != 0 {
		t.Fatalf("a new window offered %d, want 0", got)
	}
}
