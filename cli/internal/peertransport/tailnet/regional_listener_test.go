package tailnet

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"

	"tailscale.com/tailcfg"
)

type lateRelayEngine struct {
	failingRegionalEngine
	reachable atomic.Bool
	prepares  atomic.Int32
	restuns   atomic.Int32
	mu        sync.Mutex
	published []*tailcfg.DERPRegion
}

func (e *lateRelayEngine) SetRelayRegions(regions []*tailcfg.DERPRegion) error {
	e.mu.Lock()
	e.published = regions
	e.mu.Unlock()
	return nil
}
func (e *lateRelayEngine) PrepareRelay(context.Context, tailcfg.DERPRegionID) error {
	e.prepares.Add(1)
	if !e.reachable.Load() {
		return errors.New("relay did not answer")
	}
	return nil
}
func (e *lateRelayEngine) ReSTUN(string) { e.restuns.Add(1) }

// A listener restored after a reboot starts before its relays are known or answer (canary-a 2026-11-11: no relay
// presence and only a container-private endpoint for hours). Relays that arrive and come up later must be
// published, connected and re-STUNed within a bound; once connected the idle listener goes quiet again.
func TestRestoredListenerReachesRelaysThatArriveAfterStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, cfg, signer, _ := networkTestAuthority(t)
		now := time.Now().Unix()
		long := now + int64(relaylimits.MaxGrantLifetime/time.Second)
		cfg.IssuedAt, cfg.ExpiresAt, cfg.Peers[0].Scopes[0].ExpiresAt = now, now+3600, long
		if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
			t.Fatal(err)
		}
		e := &lateRelayEngine{}
		a.mu.Lock()
		a.relay.mu.Lock()
		r := a.newRegionalRecoveryLocked()
		r.engine = e
		a.relay.recovery = r
		a.relay.mu.Unlock()
		a.mu.Unlock()
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); <-r.done }()
		// As PrepareRegional starts an unbound listener: the sweep and the one-time rendezvous.
		go r.run(ctx)
		go r.prepareListener(ctx)
		time.Sleep(10 * time.Second)

		node := RegionalNode{NodeID: "relay_late", NodeGeneration: 1, ProcessEpoch: "epoch_late", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 3600, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
		candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: long, Nodes: []RegionalNode{node}}
		if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
			t.Fatal(err)
		}
		g := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: long, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(g)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		if err := a.ApplyRelayGrants(t.Context(), []string{signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		e.mu.Lock()
		published := len(e.published) == 1 && e.published[0].RegionID == regionalID(node.NodeID)
		e.mu.Unlock()
		if !published {
			t.Fatal("relays that arrived after the listener started were never published to its engine")
		}
		if e.prepares.Load() == 0 {
			t.Fatal("an unreachable relay was not retried")
		}

		e.reachable.Store(true)
		time.Sleep(30 * time.Second) // the per-relay backoff is at most 12 s
		synctest.Wait()
		if r.repair.Load() || e.restuns.Load() == 0 {
			t.Fatalf("relay came back but listener not recovered: repair=%v restuns=%d", r.repair.Load(), e.restuns.Load())
		}
		quiet := e.prepares.Load()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if got := e.prepares.Load(); got != quiet {
			t.Fatalf("a connected idle listener kept probing: %d prepares in 5 min", got-quiet)
		}
	})
}
