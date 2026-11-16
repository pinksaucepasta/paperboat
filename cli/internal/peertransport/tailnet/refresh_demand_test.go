package tailnet

import (
	"context"
	"errors"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

// The retry doubles to about a minute and is spread over half to one and a half times its base, so a
// fleet that lost the server together (a deploy) does not come back within the same ~250 ms.
func TestRefreshRetryDelayGrowsToAboutAMinute(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, base := range want {
		got := refreshRetryDelay(i + 1)
		if got < base/2 || got >= base/2*3 {
			t.Fatalf("failure %d: delay %v outside [%v, %v)", i+1, got, base/2, base/2*3)
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
	client := networkAPIFunc{configuration: func(ctx context.Context, _ string) (string, error) {
		calls.Add(1)
		reached(ctx)
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

// reached tells the renewal's trace that its request got a connection, as a real server that answers does.
func reached(ctx context.Context) {
	if trace := httptrace.ContextClientTrace(ctx); trace != nil && trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{})
	}
}

// F21-49: a renewal after one that reached no server is bounded short (it waits on a lookup or connect sent into the
// outage) but keeps the failure backoff; a local network change ends the backoff and renews at once.
func TestUnreachedRenewalIsBoundedShortAndANetworkChangeRenewsAtOnce(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	var calls, renewed atomic.Int32
	var back atomic.Bool
	var longest atomic.Int64
	client := networkAPIFunc{configuration: func(ctx context.Context, _ string) (string, error) {
		n := calls.Add(1)
		if !back.Load() {
			started := time.Now()
			<-ctx.Done() // sent into the outage: no connection, nothing comes back
			// The first attempt still had the long bound.
			if took := int64(time.Since(started)); n > 1 && took > longest.Load() {
				longest.Store(took)
			}
			return "", ctx.Err()
		}
		reached(ctx)
		renewed.Add(1)
		c.Generation++
		return networkToken(t, signer, c), nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	waitFor(t, 30*time.Second, "three renewals into the outage", func() bool { return calls.Load() >= 3 })
	if bound := time.Duration(longest.Load()); bound > unreachedAttempt+time.Second {
		t.Fatalf("an unreached renewal waited %v", bound)
	}
	time.Sleep(unreachedAttempt + 500*time.Millisecond) // let the attempt in flight end; the next waits on the backoff
	back.Store(true)
	start := time.Now()
	a.NetworkChanged()
	waitFor(t, unreachedAttempt+time.Second, "a renewal right after the network changed", func() bool { return renewed.Load() >= 1 })
	t.Logf("renewed %v after the network changed", time.Since(start).Round(time.Millisecond))
}

// A server that cannot be reached at all (its host down) is asked on the failure backoff, not every second: over ten
// minutes one engine makes a bounded number of attempts, so a fleet does not pile onto it when it returns.
func TestUnreachedServerIsAskedOnTheBackoffForTenMinutes(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	unreached := errors.New("dial tcp: i/o timeout")
	attempts, elapsed := 0, time.Duration(0)
	for failures := 1; elapsed < 10*time.Minute; failures++ {
		elapsed += unreachedAttempt + a.nextRefresh(unreached, failures, 0)
		attempts++
	}
	if attempts > 30 {
		t.Fatalf("%d attempts in ten minutes against an unreachable server", attempts)
	}
	t.Logf("%d attempts in ten minutes", attempts)
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
