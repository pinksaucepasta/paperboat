package tailnet

import (
	"context"
	"sync"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/wgengine/magicsock"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// regionRecorder records which relay regions the loop publishes and prepares.
type regionRecorder struct {
	failingRegionalEngine
	mu        sync.Mutex
	published [][]tailcfg.DERPRegionID
	prepared  []tailcfg.DERPRegionID
}

func (e *regionRecorder) SetRelayRegions(regions []*tailcfg.DERPRegion) error {
	ids := make([]tailcfg.DERPRegionID, 0, len(regions))
	for _, region := range regions {
		ids = append(ids, region.RegionID)
	}
	e.mu.Lock()
	e.published = append(e.published, ids)
	e.mu.Unlock()
	return nil
}

func (e *regionRecorder) PrepareRelay(_ context.Context, id tailcfg.DERPRegionID) error {
	e.mu.Lock()
	e.prepared = append(e.prepared, id)
	e.mu.Unlock()
	return nil
}

// overloadedCarrier is a relay carrier whose relay closed the account's carriers at its handover deadline and
// now refuses it with the non-fatal ErrOverload.
type overloadedCarrier struct{ magicsock.DERPCarrier }

func (overloadedCarrier) Connect(context.Context) error { return derpquic.ErrOverload }

// A relay leaves the account's pool (Drain and move): the next relay list already excludes it (DrainDeadline
// set) and, at close_at, it closes the account's carriers and refuses the redial with ErrOverload. magicsock
// redials the home carrier, the failed Connect is a lost path, and the loop looks at the relay list at once
// (not after its 30 s parked recheck) and prepares and publishes only the remaining relay.
func TestRelayClosingWithOverloadMovesTheListenerToAnotherRelayAtOnce(t *testing.T) {
	a, configuration, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	relay := func(id string) RegionalNode {
		return RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: "epoch_" + id, Region: "test", FailureDomain: "domain_" + id,
			Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: 443,
			State: "ready", ObservedAt: now, ExpiresAt: now + 600, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	}
	closing, other := relay("relay_a"), relay("relay_b")
	drain := now + 20
	closing.DrainDeadline = &drain
	a.regional = &RegionalCandidates{IssuedAt: now, ExpiresAt: now + 600, Nodes: []RegionalNode{closing, other}}
	a.relay.mu.Lock()
	a.relay.grants = map[string]peernetwork.RelayGrant{}
	for _, n := range []RegionalNode{closing, other} {
		a.relay.grants[n.NodeID] = peernetwork.RelayGrant{NodeID: n.NodeID, NodeGeneration: n.NodeGeneration, ProcessEpoch: n.ProcessEpoch, ExpiresAt: now + 600}
	}
	engine := &regionRecorder{}
	r := &regionalRecovery{
		authority: a, control: newRegionalControl(a.private, time.Now), engine: engine, wake: make(chan struct{}, 1), grantWake: make(chan struct{}, 1),
		done: make(chan struct{}), ready: make(chan struct{}), inbox: make(chan regionalInbound, 64),
	}
	a.relay.recovery = r
	a.relay.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	go r.run(ctx)
	defer cancel()
	time.Sleep(200 * time.Millisecond) // the loop has parked: its relays look connected

	carrier := &usageObservedCarrier{DERPCarrier: overloadedCarrier{}, authority: &a.relay, node: closing, refresh: a.RequestRefresh}
	if err := carrier.Connect(ctx); err == nil {
		t.Fatal("the closing relay admitted the carrier")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		engine.mu.Lock()
		published, prepared := engine.published, engine.prepared
		engine.mu.Unlock()
		if len(prepared) > 0 {
			want := regionalID(other.NodeID)
			if len(published) == 0 || len(published[len(published)-1]) != 1 || published[len(published)-1][0] != want || prepared[0] != want {
				t.Fatalf("published %v, prepared %v: want only %v", published, prepared, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the listener did not move to the other relay within 5 s of the closing relay refusing it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
