package tailnet

import (
	"context"
	"crypto/ed25519"
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

type countingRefreshKeys struct {
	known     map[string]ed25519.PublicKey
	pending   map[string]ed25519.PublicKey // published by the next refresh
	refreshes int
}

func (k *countingRefreshKeys) Lookup(_ context.Context, id string) (ed25519.PublicKey, bool, error) {
	key, ok := k.known[id]
	return key, ok, nil
}

func (k *countingRefreshKeys) Refresh(context.Context) error {
	k.refreshes++
	for id, key := range k.pending {
		k.known[id] = key
	}
	return nil
}

// F21-18: every configuration refresh fetched the key set (about twice a minute per engine). It is fetched
// only when a token names a key the cache does not hold: a rotation, or the cached set's lifetime ending.
func TestConfigurationRefreshFetchesKeysOnlyForAnUnknownKey(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	current := a.options.Keys.(networkTestKeys)
	keys := &countingRefreshKeys{known: map[string]ed25519.PublicKey{}, pending: map[string]ed25519.PublicKey{}}
	for id, key := range current {
		keys.known[id] = key
	}
	a.options.Keys = keys
	token := networkToken(t, signer, c)
	for range 3 {
		if err := a.knowSigningKeys(t.Context(), []string{token, ""}); err != nil {
			t.Fatal(err)
		}
	}
	if keys.refreshes != 0 {
		t.Fatalf("%d key-set fetches for a known key, want none", keys.refreshes)
	}
	for id, key := range keys.known { // the key rotates: the cache no longer holds it
		keys.pending[id] = key
		delete(keys.known, id)
	}
	if err := a.knowSigningKeys(t.Context(), []string{token}); err != nil || keys.refreshes != 1 {
		t.Fatalf("an unknown key: %d fetches, err %v; want one", keys.refreshes, err)
	}
}

// While renewals bring no usable relay, an engine in regional recovery renewed every ~9 s, and its
// recovery loop asked for a renewal every second: during a relay outage the whole fleet polled. Renewals
// now back off to about a minute and return to the observation schedule once a relay is usable.
func TestRenewalsBackOffWhileNoRelayIsUsable(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	c.IssuedAt = time.Now().Unix()
	c.ExpiresAt = c.IssuedAt + 300
	c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.relay.mu.Lock()
	a.relay.recovery = a.newRegionalRecoveryLocked()
	a.relay.mu.Unlock()
	a.mu.Unlock()
	for range 50 {
		if got := a.nextRefresh(nil, 0, 1); got < regionalUnusableRetry/2 || got >= regionalUnusableRetry*3/2 {
			t.Fatalf("first renewal without a usable relay after %v, want about %v", got, regionalUnusableRetry)
		}
		if got := a.nextRefresh(nil, 0, 10); got < regionalUnusableRetryMaximum/2 {
			t.Fatalf("renewal after ten without a usable relay came after %v, want about a minute", got)
		}
	}
}
