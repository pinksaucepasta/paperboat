package tailnet

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// relayAnswer is a control plane answering every renewal with result.
type relayAnswer struct {
	result api.PeerNetworkConfigurationResult
}

func (relayAnswer) RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return api.PeerNetworkRegistrationResult{}, nil
}
func (r relayAnswer) PeerNetworkConfiguration(context.Context, api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	return r.result, nil
}

// Behind NAT a daemon restarted while Paperboat was unreachable restored its peers but no relay, so it reached no peer
// until Paperboat answered (a Windows test VM runner -> hp container, carrier_dial timed out, 2026-11-16). The relay list and
// grants of the last answer are now restored with the configuration; a later answer that withdraws them (or a newer
// configuration generation) leaves nothing to restore.
func TestRestartWithoutPaperboatRestoresTheLastRelays(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	now := time.Now().Unix()
	node := RegionalNode{NodeID: "relay_a", NodeGeneration: 1, ProcessEpoch: "epoch_relay_a", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 3600, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: cfg.Generation, IssuedAt: now, ExpiresAt: now + 3600, Nodes: []RegionalNode{node}}
	g := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 3600, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch}
	g.Memberships = grantMemberships(cfg.Peers[0].Scopes[0])
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
	body, _ := json.Marshal(g)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	grant := signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	answered := api.PeerNetworkConfigurationResult{Configuration: networkToken(t, signer, cfg), CandidateSet: regionalToken(t, signer, candidates), RelayGrants: []string{grant}}
	if err := a.Refresh(t.Context(), relayAnswer{answered}); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()

	restartedRelays := func() int {
		t.Helper()
		next, err := NewAuthority(AuthorityOptions{Store: a.options.Store, Issuer: a.options.Issuer, Self: a.options.Self, Keys: unreachableKeys{}})
		if err != nil {
			t.Fatal(err)
		}
		defer next.Close()
		if err := next.Restore(t.Context()); err != nil {
			t.Fatal("restore:", err)
		}
		nodes, _ := next.RegionalNodes(time.Now())
		next.relay.mu.Lock()
		defer next.relay.mu.Unlock()
		if len(nodes) != len(next.relay.grants) {
			t.Fatalf("%d relays restored with %d grants", len(nodes), len(next.relay.grants))
		}
		return len(next.relay.grants)
	}
	if n := restartedRelays(); n != 1 {
		t.Fatalf("restart without Paperboat restored %d relays, want the last known one", n)
	}

	// A newer answer without relays (withdrawn, or the account's relays removed) replaces the saved ones.
	b, err := NewAuthority(AuthorityOptions{Store: a.options.Store, Issuer: a.options.Issuer, Self: a.options.Self, Keys: a.options.Keys})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Generation++
	if err := b.Refresh(t.Context(), relayAnswer{api.PeerNetworkConfigurationResult{Configuration: networkToken(t, signer, cfg)}}); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	if n := restartedRelays(); n != 0 {
		t.Fatalf("withdrawn relays came back after a restart: %d", n)
	}

	// A relay file of another configuration generation applies nothing (its claims no longer match).
	b, _ = NewAuthority(AuthorityOptions{Store: a.options.Store, Issuer: a.options.Issuer, Self: a.options.Self, Keys: a.options.Keys})
	if err := b.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.saveRelays(answered.CandidateSet, answered.RelayGrants) // generation 1 relays beside the generation 2 configuration
	_ = b.Close()
	if n := restartedRelays(); n != 0 {
		t.Fatalf("relays of an older generation were restored: %d", n)
	}
}
