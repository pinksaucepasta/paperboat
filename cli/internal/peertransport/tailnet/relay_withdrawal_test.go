package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=relay-credential-test
	"tailscale.com/tailcfg"
)

// F5-10: the host closes the sessions of a withdrawn peer, then the path is cut. A relay grant that dropped the pair
// would be presented to the relay one poll later and make it drop the pair at once, eating the close of a session the
// host had not yet closed (device: one of two plain ssh sessions survived, the close frame never arrived). The
// previous grant stays in force within the withdrawal grace; a grant that adds a pair is never held back.
func TestNarrowedRelayGrantWaitsForTheWithdrawalGrace(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	now := time.Now().Unix()
	node := RegionalNode{NodeID: "relay_withdrawal", NodeGeneration: 1, ProcessEpoch: "epoch_withdrawal", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	peerKey := cfg.Peers[0].Identity.WireGuardPublicKey
	scope := derpquic.Scope{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "terminal", Direction: "dial", Port: 443, ExpiresAt: now + 300}
	apply := func(c NetworkConfiguration, withPeer bool) string {
		t.Helper()
		if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
			t.Fatal(err)
		}
		candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: c.Issuer, Audience: "paperboat-regional-candidates", AccountID: c.Self.AccountID, EndpointID: c.Self.EndpointID, AuthorizationGeneration: c.Generation, Generation: c.Generation, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
		if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
			t.Fatal(err)
		}
		g := peernetwork.RelayGrant{Version: 1, Issuer: c.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: c.Generation, AccountID: c.Self.AccountID, EndpointID: c.Self.EndpointID, WireGuardPublicKey: c.Self.WireGuardPublicKey, CertificateFingerprint: c.Self.QUICCertificateFingerprint, QUICPublicKey: c.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch}
		if withPeer {
			g.Peers = []peernetwork.RelayPeer{{WireGuardPublicKey: peerKey, Scopes: []derpquic.Scope{scope}}}
		}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
		body, _ := json.Marshal(g)
		signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
		token := signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
		if err := a.ApplyRelayGrants(t.Context(), []string{token}); err != nil {
			t.Fatal(err)
		}
		return token
	}
	presented := func() string {
		t.Helper()
		a.relay.mu.Lock()
		a.relay.nodes = map[tailcfg.DERPRegionID]RegionalNode{regionalID(node.NodeID): node}
		a.relay.mu.Unlock()
		token, err := a.relayToken(node)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	paired := apply(cfg, true)
	if presented() != paired {
		t.Fatal("the grant of the current configuration is not the one presented")
	}
	// A session admitted under the peer's scope is open when the configuration withdraws the peer.
	a.mu.Lock()
	if _, err := a.retainScopeLocked(cfg.Peers[0].Scopes[0]); err != nil {
		a.mu.Unlock()
		t.Fatal(err)
	}
	a.mu.Unlock()
	withdrawn := cfg
	withdrawn.Generation++
	withdrawn.Peers = nil
	narrowed := apply(withdrawn, false)
	if presented() != paired {
		t.Fatal("the narrowed grant was presented while the withdrawn peer's session was open")
	}
	// The session closes: the path is cut and the narrowed grant takes over.
	a.mu.Lock()
	a.admitted = nil
	a.cutWithdrawnLocked()
	a.mu.Unlock()
	if presented() != narrowed {
		t.Fatal("the narrowed grant did not take over after the withdrawn peer was cut")
	}
	// A grant that adds the pair back is presented at once, even within a withdrawal grace.
	a.mu.Lock()
	_, _ = a.retainScopeLocked(cfg.Peers[0].Scopes[0])
	a.mu.Unlock()
	again := withdrawn
	again.Generation++
	again.Peers = cfg.Peers
	if got := apply(again, true); presented() != got {
		t.Fatal("a grant that adds a pair was held back")
	}
}
