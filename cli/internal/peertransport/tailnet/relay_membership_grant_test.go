package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"tailscale.com/types/key"
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

// F21-01: a configured scope without expiry (expires_at 0) appears in a relay grant with the grant's own expiry,
// because relays and the mint refuse peer scopes without one. That grant is accepted; any other difference is not.
func TestRelayGrantPeerScopeWithoutExpiryTakesTheGrantExpiry(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.Peers[0].Identity = networkTestBinding("cli", "cli_peer", "fd7a:115c:a1e0::3", key.NewNode())
	cfg.Peers[0].Scopes = []NetworkScope{
		{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: 0},
		{ResourceKind: "machine_access", ResourceID: "host_self", ResourceGeneration: 5, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: 0},
	}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{NodeID: "relay_untilrev", NodeGeneration: 1, ProcessEpoch: "epoch_untilrev", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	grantExpiry := now + 60
	signScopes := func(scopes ...derpquic.Scope) string {
		grant := derpquic.Grant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: grantExpiry, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Peers: []derpquic.Peer{{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey, Scopes: scopes}}}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(grant)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	sign := func(scope derpquic.Scope) string { return signScopes(scope) }
	scope := derpquic.Scope{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: grantExpiry}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(scope)}); err != nil {
		t.Fatalf("a grant carrying the grant expiry for an until-revoked scope was refused: %v", err)
	}
	// A session right (expires_at 0, not machine_network) authorizes no relay pair: a grant that still carries it,
	// with any expiry, is not what the server issues and is refused; the one without it was accepted above.
	right := derpquic.Scope{ResourceKind: "machine_access", ResourceID: "host_self", ResourceGeneration: 5, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: grantExpiry}
	if err := a.ApplyRelayGrants(t.Context(), []string{signScopes(scope, right)}); !errors.Is(err, ErrAuthority) {
		t.Errorf("a grant still carrying the session right: err = %v, want refused", err)
	}
	for name, mutate := range map[string]func(*derpquic.Scope){
		"other expiry":   func(s *derpquic.Scope) { s.ExpiresAt = grantExpiry - 1 },
		"zero in grant":  func(s *derpquic.Scope) { s.ExpiresAt = 0 },
		"other resource": func(s *derpquic.Scope) { s.ResourceID = "network_other" },
	} {
		changed := scope
		mutate(&changed)
		if err := a.ApplyRelayGrants(t.Context(), []string{sign(changed)}); !errors.Is(err, ErrAuthority) {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
	}
}
