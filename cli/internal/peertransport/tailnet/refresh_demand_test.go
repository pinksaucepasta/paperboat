package tailnet

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshRetryDelayGrowsToAboutAMinute(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, base := range want {
		got := refreshRetryDelay(i + 1)
		if got < base || got > base+base/4 {
			t.Fatalf("failure %d: delay %v outside [%v, %v]", i+1, got, base, base+base/4)
		}
	}
}

// A peer's regional probe that finds the relay health observations stale must not wait for the
// next 30-second renewal: RequestRefresh renews at once.
func TestRequestRefreshRenewsBeforeTheRegularCadence(t *testing.T) {
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
	waitFor(t, 40*time.Second, "initial renewal", func() bool { return calls.Load() >= 1 })
	time.Sleep(1200 * time.Millisecond) // past the on-demand spacing
	before := calls.Load()
	a.RequestRefresh()
	waitFor(t, 10*time.Second, "on-demand renewal", func() bool { return calls.Load() > before })
}

func TestFailingRefreshBacksOffInsteadOfRetryingEverySecond(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		calls.Add(1)
		return "", errors.New("service unavailable")
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	time.Sleep(8 * time.Second)
	a.RequestRefresh() // an on-demand request must not bypass the backoff
	time.Sleep(500 * time.Millisecond)
	// Retries at about 0, 1, 3 and 7 seconds: four attempts, not one per second.
	if got := calls.Load(); got > 5 {
		t.Fatalf("%d refresh attempts in 8.5 s against a failing server", got)
	}
}

func waitFor(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A host whose relay health observations went stale used to drop the probe silently and stay
// deaf until its next 30-second renewal; it now asks for a renewal so the peer's repeated probe
// is answered within a fraction of a second.
func TestStaleRelayObservationsMakeAHostRenewInsteadOfIgnoringTheProbe(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	r := &regionalRecovery{authority: a, control: newRegionalControl(a.private, time.Now), done: make(chan struct{}), ready: make(chan struct{}), inbox: make(chan regionalInbound, 4)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan struct{})
	go func() { defer close(finished); r.receive(ctx) }()
	r.inbox <- regionalInbound{}
	select {
	case <-a.refreshNow:
	case <-time.After(2 * time.Second):
		t.Fatal("a probe that found no usable relay did not ask for a renewal")
	}
	cancel()
	<-finished
}
