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
)

// A team member's CLI holds standing machine_access dial scopes for the shared host and no machine_network scope.
// The server's relay grant carries them stamped with the grant expiry (the relay pairs the member and the host on
// reciprocal machine_access scopes); the CLI must rebuild the same set or every refresh fails with ErrAuthority and the
// member has no relayed path to a NAT'd team machine.
func TestRelayGrantCarriesStandingTeamMachineAccessScopes(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Peers[0].Scopes = []NetworkScope{
		{ResourceKind: "machine_access", ResourceID: "machine_test", ResourceGeneration: 9, Capability: "exec", Direction: "dial", Port: 443},
		{ResourceKind: "machine_access", ResourceID: "machine_test", ResourceGeneration: 9, Capability: "managed_ssh", Direction: "dial", Port: 443},
		{ResourceKind: "machine_access", ResourceID: "machine_test", ResourceGeneration: 9, Capability: "inspector", Direction: "dial", Port: 443},
	}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	node := RegionalNode{NodeID: "relay_standing", NodeGeneration: 1, ProcessEpoch: "epoch_standing", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	grant := func(scopes ...derpquic.Scope) string {
		g := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch,
			Peers: []peernetwork.RelayPeer{{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey, Scopes: scopes}}}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(g)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	}
	scope := func(capability string, generation uint64, expiresAt int64) derpquic.Scope {
		return derpquic.Scope{ResourceKind: "machine_access", ResourceID: "machine_test", ResourceGeneration: generation, Capability: capability, Direction: "dial", Port: 443, ExpiresAt: expiresAt}
	}
	// inspector is no relay capability: it stays out of the grant.
	if err := a.ApplyRelayGrants(t.Context(), []string{grant(scope("exec", 9, now+60), scope("managed_ssh", 9, now+60))}); err != nil {
		t.Fatalf("grant with the stamped standing scopes refused: %v", err)
	}
	for name, scopes := range map[string][]derpquic.Scope{
		"unstamped":        {scope("exec", 9, 0), scope("managed_ssh", 9, 0)},
		"other generation": {scope("exec", 8, now+60), scope("managed_ssh", 8, now+60)},
		"other capability": {scope("terminal", 9, now+60), scope("managed_ssh", 9, now+60)},
		"missing scope":    {scope("exec", 9, now+60)},
		"inspector kept":   {scope("exec", 9, now+60), scope("managed_ssh", 9, now+60), scope("inspector", 9, now+60)},
	} {
		if err := a.ApplyRelayGrants(t.Context(), []string{grant(scopes...)}); !errors.Is(err, ErrAuthority) {
			t.Fatalf("%s: err = %v, want ErrAuthority", name, err)
		}
	}
}
