package tailnet

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
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

type idleRelayEngine struct{}

func (idleRelayEngine) SetRelayRegions([]*tailcfg.DERPRegion) error                   { return nil }
func (idleRelayEngine) SetPeerRelayNodes([]*tailcfg.Node) error                       { return nil }
func (idleRelayEngine) RelayTransport(tailcfg.DERPRegionID) string                    { return "" }
func (idleRelayEngine) PrepareRelay(context.Context, tailcfg.DERPRegionID) error      { return nil }
func (idleRelayEngine) SetPeerRelayRegion(key.NodePublic, tailcfg.DERPRegionID) error { return nil }
func (idleRelayEngine) SendRelayControl(key.NodePublic, tailcfg.DERPRegionID, []byte) error {
	return nil
}

// Prod 2026-10-27: relay observations age out ~15 s after every renewal, and the recovery loop then asked for a
// renewal at once: one per ~15 s per engine with direct peers. It asks only while a peer is on a regional relay.
func TestStaleRelayObservationsRenewOnlyWhileARelayIsInUse(t *testing.T) {
	for _, inUse := range []bool{false, true} {
		relayInUse(t, inUse)
		a, c, signer, _ := networkTestAuthority(t)
		if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
			t.Fatal(err)
		}
		r := &regionalRecovery{authority: a, control: newRegionalControl(a.private, time.Now), done: make(chan struct{}), ready: make(chan struct{}), inbox: make(chan regionalInbound, 4), engine: idleRelayEngine{}}
		ctx, cancel := context.WithCancel(t.Context())
		go r.run(ctx) // no regional candidates: every look finds no usable relay
		select {
		case <-a.refreshNow:
			if !inUse {
				t.Fatal("stale relay observations asked for a renewal with no peer on a relay")
			}
		case <-time.After(500 * time.Millisecond):
			if inUse {
				t.Fatal("stale relay observations did not ask for a renewal while a peer uses a relay")
			}
		}
		cancel()
		<-r.done
	}
}
