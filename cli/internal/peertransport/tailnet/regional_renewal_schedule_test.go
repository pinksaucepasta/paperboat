package tailnet

import (
	"context"
	"crypto/ed25519"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"sync/atomic"
	"tailscale.com/tailcfg"
	"testing"
	"time"
)

// pbvm4 dev-a0d9ac57: an idle machine pair fetched its signed configuration about 20 times a minute per machine
// (102 machine and 68 CLI fetches in 5 min). Every engine in regional recovery renewed before its relays' 15 s
// observations ended (every ~12 s), on top of a 24-36 s cadence. A held relay needs no fresh observation, so an idle
// engine now fetches once, then only on the jittered 5-6 min safety refresh. An update burst of server
// announcements is spaced (urgentRefreshSpacing) and coalesced: a handful of fetches, not one each.
func TestIdleEngineFetchesOnlyOnTheSafetyRefreshAndAnUpdateBurstIsCoalesced(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	now := time.Now()
	relay := RegionalNode{NodeID: "relay_held", NodeGeneration: 1, ProcessEpoch: "epoch_held", Region: "test", FailureDomain: "rack",
		Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443,
		State: "ready", ObservedAt: now.Unix() - 10, CapacityObservedAt: now.Unix() - 10, ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 10}
	a.regional = &RegionalCandidates{ExpiresAt: now.Add(peernetwork.Lifetime).Unix(), Nodes: []RegionalNode{relay}}
	a.mu.Lock()
	a.relay.mu.Lock()
	a.relay.recovery = a.newRegionalRecoveryLocked()
	a.relay.nodes = map[tailcfg.DERPRegionID]RegionalNode{regionalID(relay.NodeID): relay} // the engine holds this relay
	a.relay.mu.Unlock()
	a.mu.Unlock()
	var fetches atomic.Int32
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		fetches.Add(1)
		c.Generation++
		c.IssuedAt = time.Now().Unix()
		c.ExpiresAt = c.IssuedAt + int64(peernetwork.Lifetime/time.Second)
		c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt
		return networkToken(t, signer, c), nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	// Idle: the held relay's observations end 5 s in; 8 s later there was still only the startup fetch.
	time.Sleep(8 * time.Second)
	if n := fetches.Load(); n != 1 {
		t.Fatalf("an idle engine holding a relay fetched %d times in 8 s, want only the startup fetch", n)
	}
	if next := a.nextRefresh(nil, 0, 0); next < 5*time.Minute {
		t.Fatalf("the next idle fetch is due in %s, want the 5-6 min safety refresh", next)
	}
	// An update burst: peers re-register and the server announces each change, 40 announcements in 4 s.
	for range 20 {
		a.RequestUrgentRefresh()
		a.RequestRefresh()
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(urgentRefreshSpacing + time.Second)
	if n := fetches.Load() - 1; n < 1 || n > 4 {
		t.Fatalf("an update burst of 40 announcements in 4 s made %d fetches, want 1-4 (spaced %s)", n, urgentRefreshSpacing)
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
