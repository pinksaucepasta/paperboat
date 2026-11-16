package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"tailscale.com/types/key"
)

// Membership grants (PLAN 3b "Relay pair authorization by membership") name no peers. The client accepts one
// only if each membership is a scope its own signed configuration gives it (same resource and direction), and
// the account mesh membership makes the account's configured peers relay-control
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
	sign := func(memberships []derpquic.Scope) string {
		grant := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Memberships: memberships}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(grant)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	configured := cfg.Peers[0].Scopes[0]
	member := derpquic.Scope{ResourceKind: configured.ResourceKind, ResourceID: configured.ResourceID, ResourceGeneration: configured.ResourceGeneration, Capability: configured.Capability, Direction: configured.Direction, Port: configured.Port, ExpiresAt: configured.ExpiresAt}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign([]derpquic.Scope{member})}); err != nil {
		t.Fatalf("a membership the configuration gives was refused: %v", err)
	}
	otherDirection := member
	otherDirection.Direction = map[string]string{"dial": "accept", "accept": "dial"}[member.Direction]
	otherResource := member
	otherResource.ResourceID = "resource_not_configured"
	for name, grant := range map[string]string{
		"other direction": sign([]derpquic.Scope{otherDirection}),
		"unconfigured":    sign([]derpquic.Scope{otherResource}),
	} {
		if err := a.ApplyRelayGrants(t.Context(), []string{grant}); !errors.Is(err, ErrAuthority) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// F21-01: a configured scope without expiry (expires_at 0) appears in a relay grant with the grant's own expiry,
// because relays and the mint refuse scopes without one. That grant is accepted; a membership without expiry or for
// a resource the configuration does not give is not.
func TestRelayGrantMembershipWithoutExpiryTakesTheGrantExpiry(t *testing.T) {
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
	sign := func(scope derpquic.Scope) string {
		scopes := []derpquic.Scope{scope}
		grant := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: grantExpiry, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Memberships: scopes}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(grant)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	scope := derpquic.Scope{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: grantExpiry}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(scope)}); err != nil {
		t.Fatalf("a grant carrying the grant expiry for an until-revoked scope was refused: %v", err)
	}
	for name, mutate := range map[string]func(*derpquic.Scope){
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

// The server's membership grant carries the account mesh as one network (peernetwork.AccountNetworkID), not the
// per-pair mesh scopes of the configuration. The client accepts it in each direction its configuration holds a mesh
// scope of its own account, takes the account's peers as relay-control peers, and refuses another direction or
// another account's network.
func TestRelayGrantAccountNetworkMembershipFollowsTheConfiguredMesh(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.Peers[0].Identity = networkTestBinding("cli", "cli_peer", "fd7a:115c:a1e0::3", key.NewNode())
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443, ExpiresAt: 0}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{NodeID: "relay_account", NodeGeneration: 1, ProcessEpoch: "epoch_account", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	sign := func(memberships ...derpquic.Scope) string {
		grant := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Memberships: memberships}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(grant)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	network := derpquic.Scope{ResourceKind: "machine_network", ResourceID: peernetwork.AccountNetworkID(cfg.Self.AccountID), ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443, ExpiresAt: now + 60}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(network)}); err != nil {
		t.Fatalf("the account network membership in the configured direction was refused: %v", err)
	}
	a.relay.mu.Lock()
	controlPeers := meshControlPeers(a.relay.grants[node.NodeID], cfg)
	a.relay.mu.Unlock()
	if len(controlPeers) != 1 {
		t.Fatalf("the account network membership makes %d relay-control peers, want the configured account peer", len(controlPeers))
	}
	dial := network
	dial.Direction = "dial"
	foreign := network
	foreign.ResourceID = peernetwork.AccountNetworkID("account_other")
	for name, grant := range map[string]string{"unconfigured direction": sign(dial), "another account's network": sign(foreign)} {
		if err := a.ApplyRelayGrants(t.Context(), []string{grant}); !errors.Is(err, ErrAuthority) {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
	}
}

// A peer relay binds a peer to its disco key when the peer asks to be relayed through this endpoint (regional
// recovery). A membership grant names no peers, so the key comes from the peer's signed configuration: looked up in
// the grant's peer list it was never found and every peer-relay promotion was refused.
func TestPeerRelayDiscoComesFromTheConfigurationUnderAMembershipGrant(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.Peers[0].Identity = networkTestBinding("cli", "cli_peer", "fd7a:115c:a1e0::3", key.NewNode())
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443, ExpiresAt: 0}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{NodeID: "relay_disco", NodeGeneration: 1, ProcessEpoch: "epoch_disco", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.peerRelayDisco(node.NodeID, cfg.Peers[0]); err == nil {
		t.Fatal("a peer relay bound a peer on a relay this endpoint holds no grant for")
	}
	network := derpquic.Scope{ResourceKind: "machine_network", ResourceID: peernetwork.AccountNetworkID(cfg.Self.AccountID), ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443, ExpiresAt: now + 60}
	grant := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Memberships: []derpquic.Scope{network}}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
	body, _ := json.Marshal(grant)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	if err := a.ApplyRelayGrants(t.Context(), []string{signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))}); err != nil {
		t.Fatal(err)
	}
	disco, err := a.peerRelayDisco(node.NodeID, cfg.Peers[0])
	if err != nil {
		t.Fatalf("a peer of a membership grant has no peer-relay disco key: %v", err)
	}
	if want, _ := derpquic.ParseDiscoKey(cfg.Peers[0].Identity.DiscoPublicKey); disco != want {
		t.Fatalf("peer-relay disco key %v, want the configured %v", disco, want)
	}
}
