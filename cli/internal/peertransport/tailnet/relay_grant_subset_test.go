package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// An account with more peers than one relay grant may name gets grants for a subset of this
// endpoint's peers. Such a grant is accepted (it used to be refused for not naming every peer,
// which left the whole account without relay fallback); a grant naming an unknown peer, a peer
// twice, or other scopes than configured is still refused.
func TestRelayGrantNamingASubsetOfThePeersIsAccepted(t *testing.T) {
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
	sign := func(peers []derpquic.Peer) string {
		grant := derpquic.Grant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Peers: peers}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(grant)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	configured := func(i int) derpquic.Peer {
		peer := cfg.Peers[i]
		p := derpquic.Peer{WireGuardPublicKey: peer.Identity.WireGuardPublicKey}
		for _, s := range peer.Scopes {
			p.Scopes = append(p.Scopes, derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt})
		}
		return p
	}
	if len(cfg.Peers) == 0 {
		t.Fatal("fixture has no peers")
	}
	// A grant naming only part of the peers (here none of them) is the server's bounded subset.
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(nil)}); err != nil {
		t.Fatalf("subset grant refused: %v", err)
	}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign([]derpquic.Peer{configured(0)})}); err != nil {
		t.Fatalf("grant naming a configured peer refused: %v", err)
	}
	unknown := configured(0)
	unknown.WireGuardPublicKey = cfg.Self.WireGuardPublicKey
	changed := configured(0)
	changed.Scopes = append(changed.Scopes, changed.Scopes...)
	for name, peers := range map[string][]derpquic.Peer{
		"unknown peer": {unknown},
		"peer twice":   {configured(0), configured(0)},
		"other scopes": {changed},
	} {
		if err := a.ApplyRelayGrants(t.Context(), []string{sign(peers)}); !errors.Is(err, ErrAuthority) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// Chunks: one grant per chunk of peers for the same node add up and are sent together.
	if err := a.ApplyRelayGrants(t.Context(), []string{sign(nil), sign([]derpquic.Peer{configured(0)})}); err != nil {
		t.Fatalf("chunked grants refused: %v", err)
	}
	a.relay.mu.Lock()
	joined, merged := a.relay.tokens[node.NodeID], a.relay.grants[node.NodeID]
	a.relay.mu.Unlock()
	if strings.Count(joined, "\n") != 1 || len(merged.Peers) != 1 {
		t.Fatalf("chunks not combined: %d parts, %d peers", strings.Count(joined, "\n")+1, len(merged.Peers))
	}
	if err := a.ApplyRelayGrants(t.Context(), []string{sign([]derpquic.Peer{configured(0)}), sign([]derpquic.Peer{configured(0)})}); !errors.Is(err, ErrAuthority) {
		t.Fatalf("a peer named in two chunks: err = %v", err)
	}
}
