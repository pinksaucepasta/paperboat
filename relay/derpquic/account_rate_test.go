package derpquic

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// An account's fair-use speed on a relay is one token bucket shared by all its carriers: configured in Mbit/s, lower for the
// accounts named slow, applied live to the account's existing bucket, lifted (to the relay's own ceiling) by 0, and bounded.
func TestAccountSpeedsSetTheSharedBudgetRate(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	if err := s.SetAccountSpeed(10); err != nil {
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
	// A second of traffic is bounded by 10 Mbit/s (1.25 MB) plus a quarter second of burst, and the budget does refill.
	now, sent := time.Now(), 0
	for at := now; at.Before(now.Add(time.Second)); at = at.Add(time.Microsecond) {
		if budget.allow(at, 1400) {
			sent += 1400
		}
	}
	if sent > 1_250_000+312_500 || sent < 1_250_000 {
		t.Fatalf("one second at 10 Mbit/s forwarded %d bytes", sent)
	}
	// Naming the account slow lowers its existing budget to the second tier; unnaming returns it.
	if err := s.SetAccountSpeeds(map[string]float64{"fair": 0.512}); err != nil {
		t.Fatal(err)
	}
	if got := s.AccountMbit("fair"); got != 0.512 {
		t.Fatalf("a slow account is held to %v Mbit/s, want 0.512", got)
	}
	if got := s.AccountMbit("someone else"); got != 10 {
		t.Fatalf("another account is held to %v Mbit/s, want 10", got)
	}
	budget.mu.Lock()
	slow := budget.rate
	budget.mu.Unlock()
	if slow != 0.512e6/8 {
		t.Fatalf("the existing budget refills at %v bytes/s, want 64000", slow)
	}
	if err := s.SetAccountSpeeds(nil); err != nil || s.AccountMbit("fair") != 10 {
		t.Fatalf("clearing the slow list did not restore the normal speed: %v %v", err, s.AccountMbit("fair"))
	}
	// 0 lifts the cap to the relay's ceiling.
	if err := s.SetAccountSpeed(0); err != nil {
		t.Fatal(err)
	}
	budget.mu.Lock()
	lifted := budget.rate
	budget.mu.Unlock()
	if lifted != float64(accountBytesPerSecond) {
		t.Fatalf("lifted cap = %v bytes/s, want the ceiling %d", lifted, accountBytesPerSecond)
	}
	for _, bad := range []float64{-1, MaxAccountMbit + 1} {
		if s.SetAccountSpeed(bad) == nil || s.SetAccountSpeeds(map[string]float64{"x": bad}) == nil {
			t.Fatalf("account cap %v accepted", bad)
		}
	}
}

// An account's speed is ONE bucket across relays: the control plane gives each relay that carries it its share, and the totals across
// the relays never exceed the speed (within the shares' bursts, a quarter second each).
func TestAccountSpeedIsOneBucketAcrossRelays(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	newRelay := func(node string, share float64) *Server {
		s, err := NewServer(Verifier{Issuer: "test", NodeID: node, NodeGeneration: 1, ProcessEpoch: "epoch", Keys: map[string]ed25519.PublicKey{"test": pub}})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetAccountSpeed(10); err != nil {
			t.Fatal(err)
		}
		// What the lease says for an account on two relays: half of 10 Mbit/s on each.
		if err := s.SetAccountSpeeds(map[string]float64{"fair": share}); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.accounts["fair"] = newBudgetBurst(s.rateForLocked("fair"), accountBurst(s.rateForLocked("fair")))
		s.mu.Unlock()
		return s
	}
	a, b := newRelay("relay-a", 5), newRelay("relay-b", 5)
	now, sent := time.Now(), 0
	for at := now; at.Before(now.Add(2 * time.Second)); at = at.Add(time.Microsecond) {
		for _, s := range []*Server{a, b} {
			s.mu.Lock()
			budget := s.accounts["fair"]
			s.mu.Unlock()
			if budget.allow(at, 1400) {
				sent += 1400
			}
		}
	}
	const speed, window, burst = 1_250_000, 2, 2 * 156_250 // 10 Mbit/s, seconds, a quarter second of each 5 Mbit/s share
	if sent > speed*window+burst {
		t.Fatalf("two relays moved %d bytes in %d s for one 10 Mbit/s account, want at most %d", sent, window, speed*window+burst)
	}
	if sent < speed*window*9/10 {
		t.Fatalf("two relays moved only %d bytes in %d s: the shares under-use the account's speed", sent, window)
	}
}
