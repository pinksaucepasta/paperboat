package tailnet

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

type idleRegionalEngine struct {
	failingRegionalEngine
	sweeps  atomic.Int32
	maps    atomic.Int32
	probes  atomic.Int32
	nils    atomic.Int32 // nil relay region publications: they tear down live carriers
	replies atomic.Int32
}

func (e *idleRegionalEngine) SetRelayRegions(regions []*tailcfg.DERPRegion) error {
	e.sweeps.Add(1)
	e.maps.Add(1)
	if regions == nil {
		e.nils.Add(1)
	}
	return nil
}
func (e *idleRegionalEngine) SetPeerRelayNodes([]*tailcfg.Node) error {
	e.maps.Add(1)
	return nil
}
func (e *idleRegionalEngine) SendRelayControl(key.NodePublic, tailcfg.DERPRegionID, []byte) error {
	e.replies.Add(1)
	return nil
}
func (e *idleRegionalEngine) PrepareRelay(context.Context, tailcfg.DERPRegionID) error {
	e.probes.Add(1)
	return context.DeadlineExceeded
}

// A listening carrier must remain reachable without probing its relay shortlist
// or publishing unchanged maps while no pair needs recovery. Waking from idle must
// answer a peer's first probe at once and must not publish nil maps (that tears
// down live carriers mid-dial): observations are judged as of signing, so idle
// time alone does not make them stale.
func TestRegionalRecoveryIdleFiveMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, cfg, signer, _ := networkTestAuthority(t)
		now := time.Now().Unix()
		remote := key.NewNode()
		cfg.ExpiresAt, cfg.Peers[0].Scopes[0].ExpiresAt = now+600, now+600
		cfg.Peers[0].Identity = networkTestBinding("machine", "machine_test", "fd7a:115c:a1e0::2", remote)
		if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
			t.Fatal(err)
		}
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
		// The peer's first probe after idle is answered without waiting for a control refresh.
		probe, _, err := newRegionalControl(remote, time.Now).request(a.private.Public(), node, 1, false)
		if err != nil {
			t.Fatal(err)
		}
		if !a.relayControl(regionalID(node.NodeID), remote.Public(), probe) {
			t.Fatal("inbound relay control not accepted")
		}
		synctest.Wait()
		select {
		case <-a.refreshNow:
			t.Fatal("first inbound probe after idle waited for a control refresh")
		default:
		}
		if e.replies.Load() != 1 {
			t.Fatalf("first inbound probe after idle not answered: replies=%d", e.replies.Load())
		}
		// Idle observation expiry must not pull the regular control refresh forward.
		if delay := a.nextRefresh(nil, 0, 0); delay < 48*time.Second {
			t.Fatalf("idle refresh in %s", delay)
		}
		r.repair.Store(true)
		if delay := a.nextRefresh(nil, 0, 0); delay >= 12*time.Second {
			t.Fatalf("lost listening path used idle refresh cadence: %s", delay)
		}
		r.repair.Store(false)
		a.BindRegionalPeer(cfg.Peers[0].Identity.EndpointID)
		synctest.Wait()
		if e.sweeps.Load() == 0 {
			t.Fatal("a bound pair did not wake recovery")
		}
		if e.nils.Load() != 0 {
			t.Fatalf("post-idle sweep published nil relay regions %d times", e.nils.Load())
		}
		select {
		case <-a.refreshNow:
			t.Fatal("post-idle sweep waited for a control refresh")
		default:
		}
		t.Log("idle 300s: sweeps=0 probes=0 map publications=0; first probe answered without refresh; bind wakes recovery with no nil publication")
	})
}
