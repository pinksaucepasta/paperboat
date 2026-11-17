package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"tailscale.com/types/key"
)

// A relay grant's memberships must be exactly what the signed configuration gives the endpoint, expiry included
// (the set the server signs from, peernetwork.RelayGrantMemberships). Fewer is allowed (the server withholds peers
// its relay does not admit); a different expiry, a standing right, another resource or direction, a per-pair mesh
// scope the account network replaces, a scope of a mesh peer's session rights, or one membership twice is not.
func TestRelayGrantMembershipsMustBeExactlyTheConfiguredSet(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.Peers[0].Identity = networkTestBinding("cli", "cli_peer", "fd7a:115c:a1e0::3", key.NewNode())
	cfg.Peers[0].Scopes = []NetworkScope{
		{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: 443},
		{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443},
		{ResourceKind: "machine_access", ResourceID: "session_a", ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: time.Now().Unix() + 300},
	}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	grantExpiry := now + 60
	node := RegionalNode{NodeID: "relay_exact", NodeGeneration: 1, ProcessEpoch: "epoch_exact", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	sign := func(memberships ...derpquic.Scope) string {
		grant := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: grantExpiry, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Memberships: memberships}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(grant)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	peers := [][]derpquic.Scope{cfg.Peers[0].Scopes}
	exact := peernetwork.RelayGrantMemberships(cfg.Self.AccountID, peers, grantExpiry)
	if len(exact) != 2 || exact[0].ResourceID != peernetwork.AccountNetworkID(cfg.Self.AccountID) || exact[1].ResourceKind != "machine_network" {
		t.Fatalf("fixture memberships %+v, want the account network in both directions only", exact)
	}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(exact...)}); err != nil {
		t.Fatalf("the exact set was refused: %v", err)
	}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(exact[0])}); err != nil {
		t.Fatalf("a subset (peers the relay withholds) was refused: %v", err)
	}
	mutated := func(i int, change func(*derpquic.Scope)) derpquic.Scope { s := exact[i]; change(&s); return s }
	for name, token := range map[string]string{
		"later expiry":     sign(mutated(0, func(s *derpquic.Scope) { s.ExpiresAt++ })),
		"earlier expiry":   sign(mutated(0, func(s *derpquic.Scope) { s.ExpiresAt-- })),
		"standing":         sign(mutated(0, func(s *derpquic.Scope) { s.ExpiresAt = 0 })),
		"other direction":  sign(mutated(0, func(s *derpquic.Scope) { s.Direction = "other" })),
		"other generation": sign(mutated(0, func(s *derpquic.Scope) { s.ResourceGeneration++ })),
		"per-pair mesh scope the network replaces": sign(mutated(0, func(s *derpquic.Scope) { s.ResourceID = "network_pair" })),
		"session right of a mesh peer":             sign(derpquic.Scope{ResourceKind: "machine_access", ResourceID: "session_a", ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: cfg.Peers[0].Scopes[2].ExpiresAt}),
		"superset":                                 sign(append([]derpquic.Scope{mutated(0, func(s *derpquic.Scope) { s.ResourceID = "network_x" })}, exact...)...),
	} {
		if err := a.ApplyRelayGrants(t.Context(), []string{token}); !errors.Is(err, ErrAuthority) {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
	}
	// Parts of one grant add up; the same membership in two parts is refused.
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(exact[0]), sign(exact[1])}); err != nil {
		t.Fatalf("two parts of the exact set refused: %v", err)
	}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(exact[0]), sign(exact[0], exact[1])}); !errors.Is(err, ErrAuthority) {
		t.Fatalf("a membership named in two parts: err = %v, want refused", err)
	}
}
