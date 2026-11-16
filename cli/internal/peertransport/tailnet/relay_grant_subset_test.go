package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// An endpoint with more memberships than one relay grant may carry gets them in parts of the same grant. A part
// carrying only some of them (here none) is accepted, parts add up and are sent together, and a membership the
// configuration does not give is still refused.
func TestRelayGrantPartsOfAMembershipSubsetAreAccepted(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{NodeID: "relay_subset", NodeGeneration: 1, ProcessEpoch: "epoch_subset", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
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
	if len(cfg.Peers) == 0 {
		t.Fatal("fixture has no peers")
	}
	configured := grantMemberships(cfg.Peers[0].Scopes[0])
	// A part carrying only part of the memberships (here none of them) is the server's bounded subset.
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(nil)}); err != nil {
		t.Fatalf("subset grant refused: %v", err)
	}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(configured)}); err != nil {
		t.Fatalf("grant carrying a configured membership refused: %v", err)
	}
	unconfigured := append([]derpquic.Scope(nil), configured...)
	unconfigured[0].ResourceID = "resource_not_configured"
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(unconfigured)}); !errors.Is(err, ErrAuthority) {
		t.Fatalf("unconfigured membership: err = %v", err)
	}
	// Parts: one grant per part of the memberships for the same node add up and are sent together.
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(nil), sign(configured)}); err != nil {
		t.Fatalf("grant parts refused: %v", err)
	}
	a.relay.mu.Lock()
	joined, merged := a.relay.tokens[node.NodeID], a.relay.grants[node.NodeID]
	a.relay.mu.Unlock()
	if strings.Count(joined, "\n") != 1 || len(merged.Memberships) != 1 {
		t.Fatalf("parts not combined: %d parts, %d memberships", strings.Count(joined, "\n")+1, len(merged.Memberships))
	}
	// Exactly the relay's part limit is accepted; the check counts already installed parts.
	parts := make([]string, peernetwork.MaxGrantParts)
	for i := range parts {
		parts[i] = sign(nil)
	}
	if err := a.ApplyRelayGrants(t.Context(), parts); err != nil {
		t.Fatalf("maximum parts refused: %v", err)
	}
	if err := a.ApplyRelayGrants(t.Context(), append(parts, sign(nil))); !errors.Is(err, ErrAuthority) {
		t.Fatalf("excess part accepted: %v", err)
	}
}
