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
	"testing"
	"time"

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
	a, cfg, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	// A name that does not resolve: the redial is attempted and fails on the network instead of being refused locally.
	relay := RegionalNode{NodeID: "relay_a", NodeGeneration: 1, ProcessEpoch: "epoch_a", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{relay}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	g := derpquic.Grant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: relay.NodeID, NodeGeneration: relay.NodeGeneration, ProcessEpoch: relay.ProcessEpoch}
	peer := derpquic.Peer{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey}
	for _, s := range cfg.Peers[0].Scopes {
		peer.Scopes = append(peer.Scopes, derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt})
	}
	g.Peers = []derpquic.Peer{peer}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
	body, _ := json.Marshal(g)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
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
	// is installed either way. Put the relay in the eligible set as a fresh observation does.
	if _, err := a.ConfigureRegionalRelays(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: priv}}}); err != nil {
		t.Fatal(err)
	}
	region := regionForNode(relay)
	a.relay.mu.Lock()
	a.relay.nodes = map[tailcfg.DERPRegionID]RegionalNode{region.RegionID: relay}
	held, factory := a.relay.nodes, a.relay.factory
	a.relay.mu.Unlock()
	carrier := factory(key.NewNode(), func() *tailcfg.DERPRegion { return region })
	// The observation goes stale while the carrier redials (its relay connection died).
	a.relay.mu.Lock()
	a.relay.nodes = map[tailcfg.DERPRegionID]RegionalNode{}
	a.relay.mu.Unlock()
	t.Cleanup(func() { _ = carrier.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := carrier.Connect(ctx); err == nil {
		t.Fatal("a relay with no current authority connected")
	}
	// Fresh authority arrives; the same carrier must try the relay again, not replay the refusal.
	a.relay.mu.Lock()
	a.relay.nodes = held
	a.relay.mu.Unlock()
	retry, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := carrier.Connect(retry); errors.Is(err, derpquic.ErrAdmission) {
		t.Fatalf("the carrier kept a stale-authority refusal after fresh authority arrived: %v", err)
	}
}
