package derpquic

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"tailscale.com/types/key"

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

// A listed account speed replaces the default: a paid account listed at 100 Mbit/s on a relay whose default is Free's 10 is held to 100
// (the speed was clamped to the default, so every paid account ran at Free's speed on the relay).
func TestListedAccountSpeedAboveTheDefaultIsHonoured(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s, err := NewServer(Verifier{Issuer: "test", NodeID: "relay", NodeGeneration: 1, ProcessEpoch: "epoch", Keys: map[string]ed25519.PublicKey{"test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountSpeed(10); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountSpeeds(map[string]float64{"paid": 100, "slow": 0.512}); err != nil {
		t.Fatal(err)
	}
	for account, want := range map[string]float64{"paid": 100, "slow": 0.512, "free": 10} {
		if got := s.AccountMbit(account); got != want {
			t.Errorf("account %q is held to %v Mbit/s, want %v", account, got, want)
		}
	}
}

// Datagrams for a peer the sender may not reach cost the carrier's budget only: they add no demand to the account (a sender could
// otherwise skew how the control plane divides its account's speed among relays), while authorized traffic does.
func TestUnauthorizedDatagramsAddNoAccountDemand(t *testing.T) {
	for _, paired := range []bool{false, true} {
		f := newFixture(t)
		s, address, _ := f.start("127.0.0.1:0")
		ka, kb := key.NewNode().Public(), key.NewNode().Public()
		ca, cb := certificate(t), certificate(t)
		ga, gb := f.grant(ka, ca, "demand-a"), f.grant(kb, cb, "demand-b")
		ga.AccountID, gb.AccountID = "demandaccount", "demandaccount"
		if paired {
			pairScopes(&ga, &gb)
		}
		a, b := f.client(address, ca, ga), f.client(address, cb, gb)
		connect(t, a)
		connect(t, b)
		s.TakeAccountDemand()
		for i := 0; i < 20; i++ {
			if err := a.Send(kb, make([]byte, 1000)); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(300 * time.Millisecond)
		offered := s.TakeAccountDemand()["demandaccount"].Offered
		if paired && offered == 0 {
			t.Fatal("authorized datagrams added no demand")
		}
		if !paired && offered != 0 {
			t.Fatalf("datagrams for a peer the sender may not reach offered %d bytes of demand, want 0", offered)
		}
	}
}
