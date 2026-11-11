package tailnet

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

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
		g := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: n.NodeID, NodeGeneration: n.NodeGeneration, ProcessEpoch: n.ProcessEpoch}
		p := peernetwork.RelayPeer{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey}
		for _, s := range cfg.Peers[0].Scopes[:scopes] {
			p.Scopes = append(p.Scopes, derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt})
		}
		g.Peers = []peernetwork.RelayPeer{p}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(g)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	all := len(cfg.Peers[0].Scopes)
	// The shared grant check (the relay's own) already refuses it: no grant applies, the call says so.
	if err := a.ApplyRelayGrants(t.Context(), []string{grant(first, all)}); !errors.Is(err, ErrAuthority) {
		t.Fatalf("a grant relays refuse was applied: %v", err)
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

// One grant that does not verify (another relay process, a wrong signature, an unlisted relay, a peer the
// configuration does not name) drops only its own relay: the other relays' grants still apply. Each used to wipe
// every relay's grant, leaving the machine direct-only until the next refresh (audit A P1-5).
func TestOneBadRelayGrantDropsOnlyItsRelay(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	now := time.Now().Unix()
	node := func(id string) RegionalNode {
		return RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: "epoch_" + id, Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	}
	good, bad := node("relay_good"), node("relay_bad")
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{good, bad}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	sign := func(g peernetwork.RelayGrant, key ed25519.PrivateKey) string {
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(g)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signed)))
	}
	grant := func(n RegionalNode) peernetwork.RelayGrant {
		return peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: n.NodeID, NodeGeneration: n.NodeGeneration, ProcessEpoch: n.ProcessEpoch}
	}
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	for name, badToken := range map[string]func() []string{
		"other_process": func() []string { g := grant(bad); g.ProcessEpoch = "epoch_restarted"; return []string{sign(g, signer)} },
		"signature":     func() []string { return []string{sign(grant(bad), stranger)} },
		"unlisted":      func() []string { return []string{sign(grant(node("relay_unlisted")), signer)} },
		"unknown_peer": func() []string {
			g := grant(bad)
			g.Peers = []peernetwork.RelayPeer{{WireGuardPublicKey: cfg.Self.WireGuardPublicKey, Scopes: []derpquic.Scope{cfg.Peers[0].Scopes[0]}}}
			return []string{sign(g, signer)}
		},
		"malformed":  func() []string { return []string{"not.a.grant"} },
		"bad_second": func() []string { g := grant(bad); return []string{sign(g, signer), sign(g, stranger)} },
	} {
		t.Run(name, func(t *testing.T) {
			if err := a.ApplyRelayGrants(t.Context(), append(badToken(), sign(grant(good), signer))); err != nil {
				t.Fatalf("the good relay's grant was refused with the bad one: %v", err)
			}
			a.relay.mu.Lock()
			_, kept := a.relay.grants[good.NodeID]
			_, used := a.relay.grants[bad.NodeID]
			a.relay.mu.Unlock()
			if !kept || used {
				t.Fatalf("good relay kept=%v, bad relay used=%v", kept, used)
			}
		})
	}
}
