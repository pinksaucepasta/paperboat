package derpquic

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// An account's demand is what it offered and what the limit held back (delayed or dropped); takeDemand starts a new window.
func TestAccountBudgetCountsOfferedAndHeldBytes(t *testing.T) {
	b := newAccountBudget(1000) // 1000 B/s, burst at least two fragments
	now := time.Unix(1000, 0)
	if _, ok := b.reserve(now, 500, 0); !ok {
		t.Fatal("within the burst")
	}
	if got := b.takeDemand(); got != (AccountDemand{Offered: 500}) {
		t.Fatalf("a packet within the limit: %+v, want 500 offered, none held", got)
	}
	for b.tokens > 0 { // use up the burst
		b.reserve(now, 500, time.Hour)
	}
	b.takeDemand()
	if delay, ok := b.reserve(now, 1000, time.Hour); !ok || delay <= 0 {
		t.Fatalf("over the limit: delay %v ok %v, want a delay", delay, ok)
	}
	if got := b.takeDemand(); got != (AccountDemand{Offered: 1000, Held: 1000}) {
		t.Fatalf("a delayed packet: %+v, want 1000 offered and held", got)
	}
	if _, ok := b.reserve(now, 1000, 0); ok {
		t.Fatal("a wait past the maximum is dropped")
	}
	if got := b.takeDemand(); got != (AccountDemand{Offered: 1000, Held: 1000}) {
		t.Fatalf("a dropped packet: %+v, want 1000 offered and held", got)
	}
	if got := b.takeDemand(); got != (AccountDemand{}) {
		t.Fatalf("a new window: %+v, want nothing", got)
	}
}

// The account budget lets through relaylimits.AccountWireOverhead more than the plan speed (the relay counts wire bytes; the plan speed is payload).
func TestAccountBudgetGrossesUpThePlanSpeedForWireOverhead(t *testing.T) {
	if got := wireRate(1e6); got != 1e6*relaylimits.AccountWireOverhead {
		t.Fatalf("wireRate(1e6) = %v, want %v", got, 1e6*relaylimits.AccountWireOverhead)
	}
	if got := wireRate(accountBytesPerSecond); got != accountBytesPerSecond {
		t.Fatalf("wireRate at the ceiling = %v, want the ceiling", got)
	}
}
