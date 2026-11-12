package tailnet

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
)

// untilReplaced is a configuration as the server signs it: in force until replaced or revoked, its exp only
// bounded by the relay grants it travels with.
func untilReplaced(c NetworkConfiguration) NetworkConfiguration {
	c.IssuedAt = time.Now().Unix()
	c.ExpiresAt = c.IssuedAt + int64(relaylimits.MaxGrantLifetime/time.Second)
	c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt
	return c
}

// PR-03: an idle engine renewed every 48-72 s, 31 of 108 server requests in the per-machine rate check. With
// nothing announced it now renews only on the 15-25 minute fallback, whether or not it listens on a relay.
func TestIdleEngineMakesNoRenewalForFifteenMinutes(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, untilReplaced(c))); err != nil {
		t.Fatal(err)
	}
	for _, listening := range []bool{false, true} {
		if listening {
			a.mu.Lock()
			a.relay.mu.Lock()
			a.relay.recovery = a.newRegionalRecoveryLocked() // idle: no pair is being recovered
			a.relay.mu.Unlock()
			a.mu.Unlock()
		}
		for range 256 {
			if got := a.nextRefresh(nil, 0, 0); got < 15*time.Minute || got > 25*time.Minute {
				t.Fatalf("idle renewal (listening on a relay: %v) after %v, want 15-25 min", listening, got)
			}
		}
	}
	if got := a.nextRefresh(nil, 0, 1); got > time.Minute {
		t.Fatalf("with no usable relay the next renewal came after %v, want the regional backoff", got)
	}
	// The carrier hears only machine_access, so it keeps a short fallback of its own.
	a.options.IdleRefresh = time.Minute
	for range 256 {
		if got := a.nextRefresh(nil, 0, 0); got < 45*time.Second || got > 75*time.Second {
			t.Fatalf("an engine with a one-minute idle renewal renewed after %v, want 45-75 s", got)
		}
	}
}

// A revocation announced while the engine idles (a peer_network event -> RequestUrgentRefresh) takes effect
// within seconds, not at the idle fallback.
func TestRevocationAnnouncedToAnIdleEngineTakesEffectInSeconds(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	c = untilReplaced(c)
	var revoked atomic.Bool
	var calls atomic.Int32
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		calls.Add(1)
		next := untilReplaced(c)
		next.Generation++
		if revoked.Load() {
			next.Generation++
			next.Peers = nil
		}
		return networkToken(t, signer, next), nil
	}}
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	peer := c.Peers[0].Identity.EndpointID
	waitFor(t, 10*time.Second, "initial renewal", func() bool { return calls.Load() >= 1 })
	time.Sleep(urgentRefreshSpacing) // the engine is idle now: its next renewal is 15+ minutes away
	revoked.Store(true)
	a.RequestUrgentRefresh()
	waitFor(t, 5*time.Second, "revocation applied", func() bool {
		_, err := a.Peer(peer)
		return errors.Is(err, ErrAdmission)
	})
}

// Any renewal Paperboat answers ends "out of sync", not only the idle loop's next one 15+ minutes later: a
// per-admission renewal after an outage clears it at once.
func TestAnAnsweredRenewalEndsOutOfSync(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, untilReplaced(c))); err != nil {
		t.Fatal(err)
	}
	failing := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		return "", errors.New("dial tcp: connection refused")
	}}
	if err := a.RefreshForAdmission(t.Context(), failing); err == nil || !a.OutOfSync() {
		t.Fatalf("a failed renewal: err %v, out of sync %v; want an error and out of sync", err, a.OutOfSync())
	}
	answering := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		next := untilReplaced(c)
		next.Generation++
		return networkToken(t, signer, next), nil
	}}
	if err := a.RefreshForAdmission(t.Context(), answering); err != nil || a.OutOfSync() {
		t.Fatalf("an answered renewal: err %v, out of sync %v; want in sync", err, a.OutOfSync())
	}
}
