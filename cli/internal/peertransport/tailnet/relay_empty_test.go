package tailnet

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"math/big"
	"testing"
	"time"
)

func TestEmptyRegionalRegistryPreservesDirectAdmissionAndLaterRelayRecovery(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	candidates := RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: cfg.Issuer, Audience: "paperboat-regional-candidates", AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, AuthorizationGeneration: cfg.Generation, Generation: 1, IssuedAt: now, ExpiresAt: now + 60, Nodes: []RegionalNode{}}
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyRelayGrants(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	raw, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: priv}}}
	regions, err := a.ConfigureRegionalRelays(tlsConfig)
	if err != nil || len(regions) != 0 {
		t.Fatalf("signed empty registry prevented direct startup: %v", err)
	}
	if _, err := a.Descriptor(cfg.Peers[0].Identity.EndpointID); err != nil {
		t.Fatal("direct peer admission lost", err)
	}
	if _, err := a.ConfigureRelay("absent", tlsConfig); !errors.Is(err, ErrRegionalAuthority) {
		t.Fatal("explicit missing relay accepted", err)
	}
	descriptor, err := a.Descriptor(cfg.Peers[0].Identity.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	client, err := a.Client(descriptor, cfg.Peers[0].Identity.EndpointID)
	if err != nil {
		t.Fatal("direct-only client engine failed", err)
	}
	defer client.Close()
	if a.relay.recovery == nil || !a.relay.regionalRecoveryConfigured || a.relay.factory == nil {
		t.Fatal("empty registry omitted authenticated relay recovery")
	}

	recovery := a.relay.recovery
	node := RegionalNode{NodeID: "relay_later", NodeGeneration: 1, ProcessEpoch: "epoch_later", Region: "test", FailureDomain: "test", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "relay.example.test", EndpointQUICPort: 443, State: "ready", ObservedAt: now, ExpiresAt: now + 60, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	candidates.Generation++
	candidates.Nodes = []RegionalNode{node}
	token := regionalToken(t, signer, candidates)
	if err := a.ApplyRegionalCandidates(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	nodes, _, _, err := recovery.snapshot()
	if err != nil || len(nodes) != 0 {
		t.Fatal("registry without grant admitted relay", err)
	}
	grant := derpquic.Grant{Version: 1, Issuer: cfg.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: now + 60, Generation: cfg.Generation, AccountID: cfg.Self.AccountID, EndpointID: cfg.Self.EndpointID, WireGuardPublicKey: cfg.Self.WireGuardPublicKey, CertificateFingerprint: cfg.Self.QUICCertificateFingerprint, QUICPublicKey: cfg.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch}
	for _, peer := range cfg.Peers {
		p := derpquic.Peer{WireGuardPublicKey: peer.Identity.WireGuardPublicKey}
		for _, s := range peer.Scopes {
			p.Scopes = append(p.Scopes, derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt})
		}
		grant.Peers = append(grant.Peers, p)
	}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-relay-grant+jwt", "kid": "network_test"})
	body, _ := json.Marshal(grant)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	signed += "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(signed)))
	if err := a.ApplyRelayGrants(t.Context(), []string{signed}); err != nil {
		t.Fatal(err)
	}
	nodes, _, _, err = recovery.snapshot()
	if err != nil || len(nodes) != 1 || nodes[0].NodeID != node.NodeID || recovery != a.relay.recovery {
		t.Fatal("verified later relay did not recover on same authority", err)
	}
	// The host uses the same signed empty registry and opens its real UDP
	// endpoint without discovering any unsigned public relay.
	host, hostCfg, hostSigner, _ := networkTestAuthority(t)
	hostCfg.Self.Role = "machine"
	hostCfg.Self.MachineID = hostCfg.Self.EndpointID
	hostCfg.Self.MachineGeneration = 1
	hostCfg.Peers[0].Identity.Role = "cli"
	hostCfg.Peers[0].Identity.MachineID = ""
	hostCfg.Peers[0].Identity.MachineGeneration = 0
	hostCfg.Peers[0].Scopes[0].Direction = "accept"
	host.options.Self = hostCfg.Self
	if err := host.Apply(t.Context(), networkToken(t, hostSigner, hostCfg)); err != nil {
		t.Fatal(err)
	}
	empty := candidates
	empty.Generation = 1
	empty.Nodes = []RegionalNode{}
	if err := host.ApplyRegionalCandidates(t.Context(), regionalToken(t, hostSigner, empty)); err != nil {
		t.Fatal(err)
	}
	if _, err := host.ConfigureRegionalRelays(tlsConfig); err != nil {
		t.Fatal(err)
	}
	listener, err := host.Listen(nil)
	if err != nil {
		t.Fatal("direct-only host engine failed", err)
	}
	defer listener.Close()

	candidates.ExpiresAt = now - 1
	if err := a.ApplyRegionalCandidates(t.Context(), regionalToken(t, signer, candidates)); !errors.Is(err, ErrRegionalAuthority) {
		t.Fatal("expired registry accepted", err)
	}
	bad := []byte(token)
	bad[len(bad)-8] ^= 1
	if err := a.ApplyRegionalCandidates(t.Context(), string(bad)); !errors.Is(err, ErrRegionalAuthority) {
		t.Fatal("tampered registry accepted", err)
	}
}
