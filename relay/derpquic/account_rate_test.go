package derpquic

import (
	"testing"
	"time"

	"tailscale.com/types/key"
)

// An account's fair-use speed on a relay is one token bucket shared by all its carriers: configured in Mbit/s,
// applied live to the account's existing bucket, lifted (to the relay's own ceiling) by 0, and bounded.
func TestAccountMbitSetsTheSharedBudgetRate(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	if err := s.SetAccountMbit(20); err != nil {
		t.Fatal(err)
	}
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "rate-a"), f.grant(kb, cb, "rate-b")
	ga.AccountID, gb.AccountID = "fair", "fair"
	pairScopes(&ga, &gb)
	connect(t, f.client(address, ca, ga))
	connect(t, f.client(address, cb, gb)) // a second carrier of the same account
	s.mu.Lock()
	budget, accounts := s.accounts["fair"], len(s.accounts)
	s.mu.Unlock()
	if budget == nil || accounts != 1 {
		t.Fatalf("one account must hold one shared budget, got %d (%v)", accounts, budget)
	}
	// A second of traffic is bounded by 20 Mbit/s (2.5 MB) plus the burst, and the budget does refill.
	now, sent := time.Now(), 0
	for at := now; at.Before(now.Add(time.Second)); at = at.Add(time.Microsecond) {
		if budget.allow(at, 1400) {
			sent += 1400
		}
	}
	if sent > 2_500_000+budgetBurstBytes || sent < 2_500_000 {
		t.Fatalf("one second at 20 Mbit/s forwarded %d bytes", sent)
	}
	// Changing the cap applies to the existing budget; 0 lifts it to the relay's ceiling.
	if err := s.SetAccountMbit(0); err != nil {
		t.Fatal(err)
	}
	budget.mu.Lock()
	lifted := budget.rate
	budget.mu.Unlock()
	if lifted != float64(accountBytesPerSecond) {
		t.Fatalf("lifted cap = %v bytes/s, want the ceiling %d", lifted, accountBytesPerSecond)
	}
	for _, bad := range []float64{-1, MaxAccountMbit + 1} {
		if s.SetAccountMbit(bad) == nil {
			t.Fatalf("account cap %v accepted", bad)
		}
	}
}
