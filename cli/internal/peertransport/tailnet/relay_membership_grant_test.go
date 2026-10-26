package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// Membership grants (PLAN 3b "Relay pair authorization by membership") name no peers. The client accepts one
// only if each membership is a scope its own signed configuration gives it (same resource and direction), and
// never beside per-peer lists; the account mesh membership makes the account's configured peers relay-control
// peers, as the relay decides.
func TestRelayMembershipGrantsMustMatchTheConfiguration(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{NodeID: "relay_members", NodeGeneration: 1, ProcessEpoch: "epoch_members", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	sign := func(memberships []derpquic.Scope, peers []derpquic.Peer) string {
		grant := derpquic.Grant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Memberships: memberships, Peers: peers}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(grant)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	configured := cfg.Peers[0].Scopes[0]
	member := derpquic.Scope{ResourceKind: configured.ResourceKind, ResourceID: configured.ResourceID, ResourceGeneration: configured.ResourceGeneration, Capability: configured.Capability, Direction: configured.Direction, Port: configured.Port, ExpiresAt: configured.ExpiresAt}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign([]derpquic.Scope{member}, nil)}); err != nil {
		t.Fatalf("a membership the configuration gives was refused: %v", err)
	}
	otherDirection := member
	otherDirection.Direction = map[string]string{"dial": "accept", "accept": "dial"}[member.Direction]
	otherResource := member
	otherResource.ResourceID = "resource_not_configured"
	for name, grant := range map[string]string{
		"other direction":      sign([]derpquic.Scope{otherDirection}, nil),
		"unconfigured":         sign([]derpquic.Scope{otherResource}, nil),
		"beside per-peer list": sign([]derpquic.Scope{member}, []derpquic.Peer{{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey, Scopes: []derpquic.Scope{member}}}),
	} {
		if err := a.ApplyRelayGrants(t.Context(), []string{grant}); !errors.Is(err, ErrAuthority) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
