//go:build load

package tailnet

// Scale check (PLAN 3b, Lane W): a machine with no usable relay node (none configured in its region, all relays down,
// or hosted relays withheld for an exhausted bandwidth quota) must not ask for its signed peer configuration every
// second. The regional recovery loop requests an on-demand refresh every regionalStaleRetry (1 s) while it finds no
// node, so during a relay outage the whole fleet would poll the control plane once a second. Found on staging (pbvm4,
// no relays): the carrier of a build with 0e560ffd sent 565 POST /v1/machine-peer-network/config in 10 min.
//
//	go test -tags load -run TestLoadNoRelayNodeDoesNotPollConfigurationEverySecond -v ./internal/peertransport/tailnet/

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadNoRelayNodeDoesNotPollConfigurationEverySecond(t *testing.T) {
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
	// The configuration carries no regional candidates: no relay node is usable.
	a.relay.mu.Lock()
	r := &regionalRecovery{
		authority: a, control: newRegionalControl(a.private, time.Now), engine: &failingRegionalEngine{cause: errors.New("no relay")},
		peerID: "machine_test", done: make(chan struct{}), ready: make(chan struct{}), inbox: make(chan regionalInbound, 64),
	}
	a.relay.recovery = r
	a.relay.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.run(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	const window = 20 * time.Second
	time.Sleep(window)
	got := calls.Load()
	t.Logf("%d configuration requests in %v with no usable relay node (%.2f/s per machine)", got, window, float64(got)/window.Seconds())
	// The recovering renewal cadence is ~8–9 s; allow the first refresh plus two renewals.
	if got > 3 {
		t.Errorf("%d configuration requests in %v with no usable relay node: refreshes every second instead of backing off", got, window)
	}
}
