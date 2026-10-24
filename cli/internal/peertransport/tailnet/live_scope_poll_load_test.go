//go:build load

package tailnet

// Scale check (PLAN 3b, Lane W): an idle machine holding one live native connection (an open `pb ssh`, a terminal, a
// carrier to a peer) must not call the signed peer configuration once a second. That endpoint returns the machine's
// whole peer set, so a fleet of 100k connected machines would cost ~100k requests/s and grow with peers per account.
// Found on staging (pbvm4): one machine with a live carrier sent 549 POST /v1/machine-peer-network/config in 10 min.
//
//	go test -tags load -run TestLoadLiveScopeDoesNotPollConfigurationEverySecond -v ./internal/peertransport/tailnet/

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadLiveScopeDoesNotPollConfigurationEverySecond(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	release, err := a.TrackAdmittedScope("machine_test", "grant_test", "terminal", "dial")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
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
	const window = 20 * time.Second
	time.Sleep(window)
	got := calls.Load()
	t.Logf("%d configuration requests in %v with one live scope (%.2f/s per machine)", got, window, float64(got)/window.Seconds())
	// The regular renewal is ~30 s; allow the first one plus one renewal.
	if got > 2 {
		t.Errorf("%d configuration requests in %v with one idle live scope: polls every second instead of being notified", got, window)
	}
}
