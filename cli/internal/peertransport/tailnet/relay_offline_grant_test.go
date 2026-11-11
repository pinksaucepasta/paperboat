package tailnet

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// A relay grant outlives the configuration's refresh hint and the relay's one-minute lease (it is withdrawn by the
// relay's fence, not by running out), so an endpoint that lost Paperboat for much longer than a minute can still
// open a NEW relay carrier through the relay it already holds a grant for (F5-08 M: a NAT'd peer needs the relay).
func TestRelayGrantAndRelaySelectionSurviveALongControlOutage(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	a, cfg, signer, _ := networkTestAuthority(t)
	now := time.Now().Unix()
	long := now + int64(relaylimits.MaxGrantLifetime/time.Second)
	cfg.IssuedAt, cfg.ExpiresAt, cfg.Peers[0].Scopes[0].ExpiresAt = now, now+120, long
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	node := RegionalNode{NodeID: "relay_a", NodeGeneration: 1, ProcessEpoch: "epoch_relay_a", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: long, Nodes: []RegionalNode{node}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatalf("a candidate set as long as its grants refused: %v", err)
	}
	g := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: long, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch}
	s := cfg.Peers[0].Scopes[0]
	g.Peers = []peernetwork.RelayPeer{{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey, Scopes: []derpquic.Scope{{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt}}}}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
	body, _ := json.Marshal(g)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	if err := a.ApplyRelayGrants(t.Context(), []string{signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))}); err != nil {
		t.Fatalf("a %s grant (past the configuration's expiry and the relay's lease) refused: %v", relaylimits.MaxGrantLifetime, err)
	}
	recovery := a.newRegionalRecoveryLocked() // the daemon's recovery worker lists the relays from the set it holds
	if _, _, _, err := recovery.snapshot(); err != nil {
		t.Fatal(err)
	}
	// Paperboat has been unreachable for 10 minutes (the server clock moves on; nothing is renewed).
	serverclock.Observe(time.Now().Add(10*time.Minute).UTC().Format(http.TimeFormat), time.Now(), time.Now())
	if !a.OutOfSync() {
		t.Fatal("the configuration is still current after the outage; the test does not model one")
	}
	if nodes, err := a.RegionalNodes(serverclock.Now()); err != nil || len(nodes) != 1 {
		t.Fatalf("relay candidates after the outage: %d, err %v; want the relay that was healthy when signed", len(nodes), err)
	}
	if got, _, err := a.EligibleRegionalNodes("relay", "derp_quic", nil, serverclock.Now()); err != nil || len(got) != 1 {
		t.Fatalf("relay selection after the outage: %d, err %v", len(got), err)
	}
	if _, _, _, err := recovery.snapshot(); err != nil {
		t.Fatalf("relay recovery after the outage: %v", err)
	}
	if token, err := a.relayToken(node); err != nil || token == "" {
		t.Fatalf("relay grant for a new carrier after the outage: %q, %v", token, err)
	}
}
