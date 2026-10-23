package tailnet

import (
	"context"
	"testing"
	"time"
)

// Relays went unusable for ~2 s every ~20 s on hp: recovery renewed 9.2-9.8 s after the last
// renewal, but the signed observations it got were already up to 5 s old and end 15 s after
// they were made, so the next renewal often came after they had ended.
func TestRegionalRenewalFollowsTheEarliestObservation(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	at := func(health, capacity int64) RegionalNode {
		return RegionalNode{ObservedAt: now.Unix() - health, CapacityObservedAt: now.Unix() - capacity}
	}
	for _, c := range []struct {
		name  string
		nodes []RegionalNode
		want  time.Duration
	}{
		{"fresh at issuance", []RegionalNode{at(0, 0)}, 12 * time.Second},
		{"five seconds old", []RegionalNode{at(5, 5)}, 7 * time.Second},
		{"earliest node decides", []RegionalNode{at(1, 1), at(6, 2)}, 6 * time.Second},
		{"older capacity observation decides", []RegionalNode{at(2, 8)}, 4 * time.Second},
		{"already old stays bounded", []RegionalNode{at(13, 13)}, regionalMinimumRenewal},
	} {
		if got := regionalRenewalAfter(c.nodes, now); got != c.want {
			t.Errorf("%s: renew after %v, want %v", c.name, got, c.want)
		}
	}
	if _, ok := (&Authority{}).regionalRenewalDelay(now); ok {
		t.Error("an authority without eligible relays scheduled an observation renewal")
	}
}

// On pbvm2 an idle machine withdrew every relay region, and with it every relay connection, about every 17 s:
// the refresh scheduled the regional renewal before the relays' observations ended, but the configuration it
// had just applied woke the loop again, and that wake re-armed the timer at the 24-36 s cadence. A renewal
// must come before the observations end, whichever path armed the timer.
func TestRegionalRecoveryRenewalSurvivesTheAppliedConfigurationWake(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	now := time.Now()
	// Observed 10 s ago: it ends in 5 s, so the renewal is due after the 4 s minimum.
	a.regional = &RegionalCandidates{ExpiresAt: now.Add(time.Minute).Unix(), Nodes: []RegionalNode{{
		NodeID: "relay_renewal", NodeGeneration: 1, ProcessEpoch: "epoch_renewal", Region: "test", FailureDomain: "rack",
		Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443,
		State: "ready", ObservedAt: now.Unix() - 10, CapacityObservedAt: now.Unix() - 10, ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 10,
	}}}
	a.mu.Lock()
	a.relay.mu.Lock()
	a.relay.recovery = a.newRegionalRecoveryLocked()
	a.relay.mu.Unlock()
	a.mu.Unlock()
	refreshed := make(chan struct{}, 8)
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		c.Generation++
		c.IssuedAt = time.Now().Unix()
		c.ExpiresAt = c.IssuedAt + 60
		c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt
		refreshed <- struct{}{}
		return networkToken(t, signer, c), nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	<-refreshed
	select {
	case <-refreshed:
	case <-time.After(7 * time.Second):
		t.Fatal("no renewal before the relays' observations ended; the applied configuration's wake re-armed the regular cadence")
	}
}
