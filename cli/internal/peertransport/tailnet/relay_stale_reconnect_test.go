package tailnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// pbmac 2026-11-05 (dev-9cda93a1c): one engine's relay carrier failed every send for 15 minutes while the
// host engine on the same Mac used the same relay, and only a daemon restart cured it. A carrier that
// redials while this machine's relay authority is momentarily not current (an idle daemon's health
// observation is 15 s fresh and renewed about every 30 s; a black hole kills the relay connection) was
// refused by our own credential callback, and derpquic keeps an admission refusal for the carrier's
// lifetime. A local "not current yet" must be retried once authority is fresh, as Tailscale's DERP
// client redials after every failure (wgengine/magicsock/derp.go runDerpReader).
func TestRelayCarrierRedialsAfterAStaleAuthorityRefusal(t *testing.T) {
	f := newUnlistedRelayFixture(t)
	f.list(t, true)
	carrier := f.carrier(t)
	// The relay drops out of the current set while the carrier redials (its relay connection died).
	f.list(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := carrier.Connect(ctx); err == nil {
		t.Fatal("a relay with no current authority connected")
	}
	// Fresh authority arrives; the same carrier must try the relay again, not replay the refusal.
	f.list(t, true)
	retry, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := carrier.Connect(retry); errors.Is(err, derpquic.ErrAdmission) {
		t.Fatalf("the carrier kept a stale-authority refusal after fresh authority arrived: %v", err)
	}
}

// magicsock builds a region's carrier lazily, on first use, and keeps it while the region's definition is
// unchanged. A carrier built while its relay was momentarily missing from the current set took an empty
// relay (no address, no pin) and stayed unusable after the relay was listed again. It uses the relay the
// region was defined for.
func TestRelayCarrierBuiltWhileItsRelayIsUnlistedUsesTheRelay(t *testing.T) {
	f := newUnlistedRelayFixture(t)
	f.list(t, true)
	f.list(t, false)
	carrier := f.carrier(t)
	f.list(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := carrier.Connect(ctx)
	if errors.Is(err, derpquic.ErrAdmission) || err == nil || !strings.Contains(err.Error(), f.relay.EndpointHost) {
		t.Fatalf("a carrier built while its relay was unlisted did not dial the relay once listed: %v", err)
	}
}

type unlistedRelayFixture struct {
	a          *Authority
	relay      RegionalNode
	region     *tailcfg.DERPRegion
	list       func(t *testing.T, listed bool)
	generation uint64
}

func (f *unlistedRelayFixture) carrier(t *testing.T) interface{ Connect(context.Context) error } {
	t.Helper()
	f.a.relay.mu.Lock()
	factory := f.a.relay.factory
	f.a.relay.mu.Unlock()
	carrier := factory(key.NewNode(), func() *tailcfg.DERPRegion { return f.region })
	t.Cleanup(func() { _ = carrier.Close() })
	return carrier
}

// newUnlistedRelayFixture is an authority with one granted relay whose presence in the current relay set the
// test controls through signed candidate sets and recovery's own snapshot, as the daemon's refresh does. A
// name that does not resolve makes a dial attempt fail on the network instead of being refused locally.
func newUnlistedRelayFixture(t *testing.T) *unlistedRelayFixture {
	t.Helper()
	a, cfg, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	f := &unlistedRelayFixture{a: a}
	f.relay = RegionalNode{NodeID: "relay_a", NodeGeneration: 1, ProcessEpoch: "epoch_a", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	f.region = regionForNode(f.relay)
	recovery := a.newRegionalRecoveryLocked()
	f.list = func(t *testing.T, listed bool) {
		t.Helper()
		f.generation++
		nodes := []RegionalNode{}
		if listed {
			nodes = append(nodes, f.relay)
		}
		candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: f.generation, IssuedAt: now, ExpiresAt: now + 60, Nodes: nodes}
		if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := recovery.snapshot(); err != nil {
			t.Fatal(err)
		}
	}
	g := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: f.relay.NodeID, NodeGeneration: f.relay.NodeGeneration, ProcessEpoch: f.relay.ProcessEpoch}
	peer := peernetwork.RelayPeer{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey}
	for _, s := range cfg.Peers[0].Scopes {
		peer.Scopes = append(peer.Scopes, derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt})
	}
	g.Peers = []peernetwork.RelayPeer{peer}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
	body, _ := json.Marshal(g)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	f.list(t, true)
	if err := a.ApplyRelayGrants(t.Context(), []string{signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))}); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	// The grant is not bound to this throwaway certificate, so configuration starts direct-only; the factory
	// is installed either way, and the test lists the relay itself.
	if _, err := a.ConfigureRegionalRelays(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: priv}}}); err != nil {
		t.Fatal(err)
	}
	return f
}
