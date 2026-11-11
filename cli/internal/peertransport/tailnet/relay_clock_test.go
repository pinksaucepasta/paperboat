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
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// F21-02: the network configuration, regional candidates (60 s) and relay grants are server-signed. A machine
// 10 min ahead read them as expired and 10 min behind as issued in the future, so it lost its peers and
// every relay; judged on the server clock it keeps both.
func TestRelayGrantsAreJudgedOnTheServerClock(t *testing.T) {
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	for name, localAhead := range map[string]time.Duration{"local 10 min ahead": 10 * time.Minute, "local 10 min behind": -10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			serverclock.Observe(time.Now().Add(-localAhead).UTC().Format(http.TimeFormat), time.Now(), time.Now())
			a, cfg, signer, _ := networkTestAuthority(t)
			now := serverclock.Now().Unix()
			cfg.IssuedAt, cfg.ExpiresAt, cfg.Peers[0].Scopes[0].ExpiresAt = now, now+300, now+300
			if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
				t.Fatalf("server-valid network configuration refused: %v", err)
			}
			node := RegionalNode{NodeID: "relay_a", NodeGeneration: 1, ProcessEpoch: "epoch_relay_a", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
			candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{node}}
			if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
				t.Fatalf("server-valid regional candidates refused: %v", err)
			}
			g := peernetwork.RelayGrant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch}
			s := cfg.Peers[0].Scopes[0]
			g.Peers = []peernetwork.RelayPeer{{WireGuardPublicKey: cfg.Peers[0].Identity.WireGuardPublicKey, Scopes: []derpquic.Scope{{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt}}}}
			header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
			body, _ := json.Marshal(g)
			signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
			if err := a.ApplyRelayGrants(t.Context(), []string{signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))}); err != nil {
				t.Fatalf("server-valid relay grant refused: %v", err)
			}
			if nodes, err := a.RegionalNodes(serverclock.Now()); err != nil || len(nodes) != 1 {
				t.Fatalf("relays under skew: %d eligible, err %v; want the one granted relay", len(nodes), err)
			}
			a.relay.mu.Lock()
			held := a.relay.grants[node.NodeID].ExpiresAt > serverclock.Now().Unix()
			a.relay.mu.Unlock()
			if !held {
				t.Fatal("the relay grant is not held as live under skew")
			}
			if _, err := a.Descriptor(cfg.Peers[0].Identity.EndpointID); err != nil {
				t.Fatalf("direct admission lost under skew: %v", err)
			}
		})
	}
}
