package tailnet

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// A grant the relay would refuse (here: more than its 128 scopes in one grant, as until_revoked
// session rights produced on prod) makes only that relay unusable: with every relay refused the
// engine starts direct-only instead of failing the runtime and with it pairing ("relay
// authorization rejected", pbvm2 2026-10-26). An explicitly requested relay still reports it.
func TestARefusedRelayGrantLeavesDirectAdmission(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	now := time.Now().Unix()
	base := cfg.Peers[0].Scopes[0]
	for i := range 130 {
		scope := base
		scope.ResourceID = fmt.Sprintf("grant_%03d", i)
		cfg.Peers[0].Scopes = append(cfg.Peers[0].Scopes, scope)
	}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	node := func(id string) RegionalNode {
		return RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: "epoch_" + id, Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	}
	first := node("relay_a")
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{first}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	grant := func(n RegionalNode, scopes int) string {
		g := derpquic.Grant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: n.NodeID, NodeGeneration: n.NodeGeneration, ProcessEpoch: n.ProcessEpoch}
		p := derpquic.Peer{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey}
		for _, s := range cfg.Peers[0].Scopes[:scopes] {
			p.Scopes = append(p.Scopes, derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt})
		}
		g.Peers = []derpquic.Peer{p}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(g)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	all := len(cfg.Peers[0].Scopes)
	if err := a.ApplyRelayGrants(t.Context(), []string{grant(first, all)}); err != nil {
		t.Fatalf("the grants were refused before relay verification: %v", err)
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
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: priv}}}
	regions, err := a.ConfigureRegionalRelays(tlsConfig)
	if err != nil || len(regions) != 0 {
		t.Fatalf("a refused grant: regions %d, err %v; want direct-only startup", len(regions), err)
	}
	if _, err := a.Descriptor(cfg.Peers[0].Identity.EndpointID); err != nil {
		t.Fatal("direct admission lost with every relay refused:", err)
	}
	if _, err := a.ConfigureRelay(first.NodeID, tlsConfig); err == nil {
		t.Fatal("an explicit relay with a refused grant was configured")
	}
}
