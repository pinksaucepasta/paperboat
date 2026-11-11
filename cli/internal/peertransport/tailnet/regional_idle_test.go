package tailnet

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"tailscale.com/tailcfg"
)

type idleRegionalEngine struct {
	failingRegionalEngine
	sweeps atomic.Int32
	maps   atomic.Int32
	probes atomic.Int32
}

func (e *idleRegionalEngine) SetRelayRegions([]*tailcfg.DERPRegion) error {
	e.sweeps.Add(1)
	e.maps.Add(1)
	return nil
}
func (e *idleRegionalEngine) SetPeerRelayNodes([]*tailcfg.Node) error {
	e.maps.Add(1)
	return nil
}
func (e *idleRegionalEngine) PrepareRelay(context.Context, tailcfg.DERPRegionID) error {
	e.probes.Add(1)
	return context.DeadlineExceeded
}

// A listening carrier must remain reachable without probing its relay shortlist
// or publishing unchanged maps while no pair needs recovery.
func TestRegionalRecoveryIdleFiveMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, cfg, signer, _ := networkTestAuthority(t)
		if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
			t.Fatal(err)
		}
		now := time.Now().Unix()
		node := RegionalNode{NodeID: "relay_idle", NodeGeneration: 1, ProcessEpoch: "epoch_idle", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 600, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
		a.regional = &RegionalCandidates{IssuedAt: now, ExpiresAt: now + 600, Nodes: []RegionalNode{node}}
		a.relay.grants = map[string]derpquic.Grant{node.NodeID: {NodeID: node.NodeID, NodeGeneration: 1, ProcessEpoch: node.ProcessEpoch, ExpiresAt: now + 600}}
		e := &idleRegionalEngine{}
		r := a.newRegionalRecoveryLocked()
		r.engine = e
		a.relay.recovery = r
		ctx, cancel := context.WithCancel(t.Context())
		go r.run(ctx)
		defer func() { cancel(); <-r.done }()
		synctest.Wait()
		for range 100 {
			// Even network notifications cannot turn an unbound listener into a probe loop.
			r.wake <- struct{}{}
			time.Sleep(3 * time.Second)
		}
		synctest.Wait()
		if e.sweeps.Load() != 0 || e.probes.Load() != 0 || e.maps.Load() != 0 {
			t.Fatalf("idle 300s: sweeps=%d probes=%d map publications=%d", e.sweeps.Load(), e.probes.Load(), e.maps.Load())
		}
		// Idle observation expiry must not pull the regular control refresh forward.
		if delay := a.nextRefresh(nil, 0, 0); delay < 48*time.Second {
			t.Fatalf("idle refresh in %s", delay)
		}
		a.BindRegionalPeer(cfg.Peers[0].Identity.EndpointID)
		synctest.Wait()
		if e.sweeps.Load() == 0 {
			t.Fatal("a bound pair did not wake recovery")
		}
		t.Log("idle 300s: sweeps=0 probes=0 map publications=0; bind wakes recovery")
	})
}
