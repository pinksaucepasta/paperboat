package tailnet

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

// Tests in this package exercise the triggers themselves; the shared budget is tested on its own below.
func TestMain(m *testing.M) {
	renewalBudget = &tokenBucket{refill: time.Nanosecond, burst: 1e9}
	os.Exit(m.Run())
}

func useBudget(t *testing.T, b *tokenBucket) {
	previous := renewalBudget
	renewalBudget = b
	t.Cleanup(func() { renewalBudget = previous })
}

// A storm of on-demand requests from two engines stays within one renewal per refill plus the burst: at the
// production 12 s refill that is at most 5 a minute per process, 10 per machine.
func TestRenewalStormFromEveryEngineStaysWithinTheBudget(t *testing.T) {
	useBudget(t, &tokenBucket{refill: 1500 * time.Millisecond, burst: 2})
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	var done []chan error
	var engines []*Authority
	for range 2 {
		a, c, signer, _ := networkTestAuthority(t)
		if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
			t.Fatal(err)
		}
		client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
			calls.Add(1)
			c.Generation++
			return networkToken(t, signer, c), nil
		}}
		ch := make(chan error, 1)
		go func() { ch <- a.Run(ctx, client, nil) }()
		done, engines = append(done, ch), append(engines, a)
	}
	defer func() {
		cancel()
		for _, ch := range done {
			<-ch
		}
	}()
	start := time.Now()
	for time.Since(start) < 6*time.Second {
		for _, a := range engines {
			a.RequestRefresh()
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Unbudgeted the two engines would renew 12 times (once a second each); the budget allows the burst
	// of 2 plus one per 1.5 s refill, 6 in 6 s.
	if got := calls.Load(); got > 7 || got < 4 {
		t.Fatalf("%d renewals in 6 s from two storming engines, want at most 6", got)
	}
}

// A request waiting on the budget is coalesced, never dropped: the renewal still happens once a token is back.
func TestRenewalOverBudgetIsDeferredNotDropped(t *testing.T) {
	b := &tokenBucket{refill: 300 * time.Millisecond, burst: 1}
	useBudget(t, b)
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		calls.Add(1)
		c.Generation++
		return networkToken(t, signer, c), nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	waitFor(t, 5*time.Second, "first renewal", func() bool { return calls.Load() >= 1 })
	time.Sleep(onDemandRefreshSpacing) // past the on-demand spacing; the bucket is now refilled
	b.take(time.Now())                 // another engine spent the token
	before := calls.Load()
	a.RequestRefresh()
	waitFor(t, 5*time.Second, "deferred renewal", func() bool { return calls.Load() > before })
}

// A 429 holds every engine's renewals for the server's Retry-After.
func TestRateLimitedRenewalHoldsTheBudgetForRetryAfter(t *testing.T) {
	b := &tokenBucket{refill: time.Millisecond, burst: 5}
	useBudget(t, b)
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		return "", &api.APIError{Status: 429, Code: "rate_limited", RetryAfter: 7 * time.Second}
	}}
	_ = a.Refresh(t.Context(), client)
	if wait := b.take(time.Now()); wait < 6*time.Second || wait > 7*time.Second {
		t.Fatalf("budget wait after a 429 with Retry-After 7s = %v", wait)
	}
}
