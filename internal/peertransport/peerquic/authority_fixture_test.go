package peerquic_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tailcfg"
)

type meshFixtureKeys map[string]ed25519.PublicKey

func (k meshFixtureKeys) Lookup(_ context.Context, id string) (ed25519.PublicKey, bool, error) {
	p, ok := k[id]
	return p, ok, nil
}

type meshFixtureSecrets map[string]string

func (s meshFixtureSecrets) Get(k string) (string, error) {
	v, ok := s[k]
	if !ok {
		return "", config.ErrSecretNotFound
	}
	return v, nil
}
func (s meshFixtureSecrets) Set(k, v string) error { s[k] = v; return nil }
func (s meshFixtureSecrets) Delete(k string) error { delete(s, k); return nil }

type meshAuthorityFixture struct {
	options   tailnet.AuthorityOptions
	authority *tailnet.Authority
	binding   tailnet.NetworkBinding
	token     string
}

// Each endpoint receives a signed snapshot and owns a real engine. Reopening
// reuses persisted key custody but replaces the complete runtime, as a restart does.
func newMeshAuthorityFixture(t *testing.T, keys meshFixtureKeys, index int) *meshAuthorityFixture {
	t.Helper()
	id, role := fmt.Sprintf("cli_%d", index), "cli"
	if index == 0 {
		id, role = "machine_test", "machine"
	}
	fingerprint := sha256.Sum256([]byte(id))
	b := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: id, Role: role, EndpointGeneration: 1, QUICCertificateFingerprint: hex.EncodeToString(fingerprint[:]), QUICPublicKey: base64.RawURLEncoding.EncodeToString(fingerprint[:]), VirtualAddress: fmt.Sprintf("fd7a:115c:a1e0::%x", index+1)}
	if index == 0 {
		b.MachineID, b.MachineGeneration = id, 1
	}
	f := &meshAuthorityFixture{options: tailnet.AuthorityOptions{Store: config.ProfileStore{Path: t.TempDir(), Secrets: meshFixtureSecrets{}}, Issuer: "https://api.example.test", Self: b, Keys: keys}}
	var err error
	f.authority, err = tailnet.NewAuthority(f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.authority.Close() })
	b.WireGuardPublicKey, b.KeyGeneration, err = f.authority.PrepareKey(false)
	if err != nil {
		t.Fatal(err)
	}
	b.KeyGeneration++
	b.DiscoPublicKey, err = f.authority.DiscoveryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	f.binding = b
	return f
}
func (f *meshAuthorityFixture) apply(t *testing.T, signer ed25519.PrivateKey, peers ...tailnet.NetworkBinding) {
	t.Helper()
	now := time.Now().Unix()
	cfg := tailnet.NetworkConfiguration{Version: 1, Issuer: f.options.Issuer, Audience: "paperboat-network", IssuedAt: now, ExpiresAt: now + 300, Generation: 1, Self: f.binding}
	direction := "dial"
	if f.binding.Role == "machine" {
		direction = "accept"
	}
	for _, peer := range peers {
		cfg.Peers = append(cfg.Peers, tailnet.NetworkPeer{Identity: peer, Scopes: []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "terminal", Direction: direction, Port: tailnet.NetworkPort, ExpiresAt: now + 300}}})
	}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-network-config+jwt", "kid": "mesh_test"})
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	f.token = unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(unsigned)))
	if err := f.authority.Apply(t.Context(), f.token); err != nil {
		t.Fatal(err)
	}
}
func (f *meshAuthorityFixture) restart(t *testing.T) {
	t.Helper()
	if err := f.authority.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.authority, err = tailnet.NewAuthority(f.options)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.authority.Apply(t.Context(), f.token); err != nil {
		t.Fatal(err)
	}
}
func meshQUICFixture(t *testing.T, region *tailcfg.DERPRegion, count int) (*tailnet.UDPServer, []*meshAuthorityFixture) {
	t.Helper()
	public, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := meshFixtureKeys{"mesh_test": public}
	host := newMeshAuthorityFixture(t, keys, 0)
	if _, err := host.authority.Listen(region); err == nil {
		t.Fatal("unsigned authority accepted")
	}
	clients := make([]*meshAuthorityFixture, count)
	peers := make([]tailnet.NetworkBinding, count)
	for i := range clients {
		clients[i] = newMeshAuthorityFixture(t, keys, i+1)
		peers[i] = clients[i].binding
	}
	host.apply(t, signer, peers...)
	for _, client := range clients {
		client.apply(t, signer, host.binding)
	}
	server, err := host.authority.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	return server, clients
}
