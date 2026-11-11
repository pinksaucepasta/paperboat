package tailnet

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"tailscale.com/tailcfg"
)

type fatalPrepareError struct{}

func (fatalPrepareError) Error() string { return "refused" }
func (fatalPrepareError) Fatal() bool   { return true }

type countingRefusingEngine struct {
	failingRegionalEngine
	prepares atomic.Int32
}

func (e *countingRefusingEngine) PrepareRelay(context.Context, tailcfg.DERPRegionID) error {
	e.prepares.Add(1)
	return fatalPrepareError{}
}

// After every relay refused admission the recovery sleeps regionalProbeDelay; a grant that lands meanwhile
// must make the next sweep run at once (Tailscale re-STUNs immediately on a DERP map update).
func TestNewRelayGrantWakesSleepingRecoverySweep(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	now := time.Now().Unix()
	long := now + int64(relaylimits.MaxGrantLifetime/time.Second)
	cfg.IssuedAt, cfg.ExpiresAt, cfg.Peers[0].Scopes[0].ExpiresAt = now, now+120, long
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	node := RegionalNode{NodeID: "relay_a", NodeGeneration: 1, ProcessEpoch: "epoch_relay_a", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: long, Nodes: []RegionalNode{node}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	grant := func() string {
		g := derpquic.Grant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: long, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(g)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	if err := a.ApplyRelayGrants(t.Context(), []string{grant()}); err != nil {
		t.Fatal(err)
	}
	engine := &countingRefusingEngine{}
	a.mu.Lock()
	a.relay.mu.Lock()
	r := a.newRegionalRecoveryLocked()
	r.engine = engine
	a.relay.recovery = r
	a.relay.mu.Unlock()
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); <-r.done }()
	go r.run(ctx)
	waitFor := func(what string, limit time.Duration, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if ok() {
				return
			}
		}
		t.Fatalf("%s did not happen within %s", what, limit)
	}
	waitFor("refused admission", time.Second, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.status.Reason == "regional_admission_denied"
	})
	// The refusal fence lasts under 0.75 s here; the sleeping timer 2.4 s or more.
	time.Sleep(900 * time.Millisecond)
	before := engine.prepares.Load()
	if err := a.ApplyRelayGrants(t.Context(), []string{grant()}); err != nil {
		t.Fatal(err)
	}
	waitFor("a sweep after the new grant", time.Second, func() bool { return engine.prepares.Load() > before })
}
