package tailnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=authority-tests
	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
)

type networkTestKeys map[string]ed25519.PublicKey

type networkAPIFunc struct {
	register      func(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error)
	configuration func(context.Context, string) (string, error)
}

func (n networkAPIFunc) RegisterPeerNetwork(ctx context.Context, r api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return n.register(ctx, r)
}
func (n networkAPIFunc) PeerNetworkConfiguration(ctx context.Context, op string) (api.PeerNetworkConfigurationResult, error) {
	token, err := n.configuration(ctx, op)
	return api.PeerNetworkConfigurationResult{Configuration: token}, err
}

func TestNetworkRegistrationLostResponseAndRefreshRecovery(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	calls := 0
	var previous api.PeerNetworkRegistration
	client := networkAPIFunc{
		register: func(_ context.Context, r api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
			calls++
			if calls == 2 && r != previous {
				t.Error("interrupted registration did not replay exact operation")
			}
			previous = r
			c.Self.KeyGeneration = r.ExpectedKeyGeneration + 1
			c.Self.WireGuardPublicKey = r.WireGuardPublicKey
			if calls == 1 {
				return api.PeerNetworkRegistrationResult{}, errors.New("lost response")
			}
			return api.PeerNetworkRegistrationResult{KeyGeneration: c.Self.KeyGeneration, VirtualAddress: c.Self.VirtualAddress}, nil
		},
		configuration: func(context.Context, string) (string, error) { c.Generation++; return networkToken(t, signer, c), nil },
	}
	if a.Register(t.Context(), client, false) == nil {
		t.Fatal("lost response not reported")
	}
	if err := a.Register(t.Context(), client, false); err != nil {
		t.Fatal(err)
	}
	if err := a.Register(t.Context(), client, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("ordinary refresh rotated a healthy key")
	}
	if err := a.Register(t.Context(), client, true); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || c.Self.KeyGeneration != 2 {
		t.Fatal("deliberate rotation did not advance binding")
	}
	client.configuration = func(context.Context, string) (string, error) {
		return "", &api.APIError{Status: 503, Code: "peer_network_unavailable"}
	}
	if a.Refresh(t.Context(), client) == nil || !a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("partition discarded still-valid authority")
	}
	client.configuration = func(context.Context, string) (string, error) {
		return "", &api.APIError{Status: 403, Code: "peer_network_denied"}
	}
	if a.Refresh(t.Context(), client) == nil || a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("explicit denial retained authority")
	}
	client.configuration = func(context.Context, string) (string, error) { c.Generation++; return networkToken(t, signer, c), nil }
	if err := a.Refresh(t.Context(), client); err != nil || !a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("fresh authorized recovery failed", err)
	}
}

func TestNetworkRunCloseCancelsPendingRefresh(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	started := make(chan struct{})
	finished := make(chan error, 1)
	client := networkAPIFunc{configuration: func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	go func() { finished <- a.Run(t.Context(), client, nil) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh not started")
	}
	_ = a.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("close did not cancel pending refresh")
	}
}

func TestNetworkRefreshWaitRespectsContextCancellation(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		close(started)
		<-release
		return "", errors.New("released refresh")
	}}
	go func() { firstDone <- a.Refresh(t.Context(), client) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not acquire serialization slot")
	}

	waitCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.Refresh(waitCtx, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting refresh error = %v, want context cancellation", err)
	}
	close(release)
	if err := <-firstDone; err == nil {
		t.Fatal("released refresh unexpectedly succeeded")
	}
}

func (k networkTestKeys) Lookup(_ context.Context, id string) (ed25519.PublicKey, bool, error) {
	p, ok := k[id]
	return p, ok, nil
}

type networkTestStore struct {
	values map[string]string
	fail   bool
	onSet  func()
}

func (s *networkTestStore) Get(k string) (string, error) {
	v, ok := s.values[k]
	if !ok {
		return "", config.ErrSecretNotFound
	}
	return v, nil
}
func (s *networkTestStore) Set(k, v string) error {
	if s.onSet != nil {
		s.onSet()
	}
	if s.fail {
		return errors.New("injected custody failure")
	}
	s.values[k] = v
	return nil
}
func (s *networkTestStore) Delete(k string) error { delete(s.values, k); return nil }
func networkToken(t *testing.T, priv ed25519.PrivateKey, c NetworkConfiguration) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-network-config+jwt", "kid": "network_test"})
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	data := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return data + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(data)))
}
func networkTestBinding(role, id, addr string, k key.NodePrivate) NetworkBinding {
	p := k.Public().Raw32()
	fp := sha256.Sum256([]byte("quic:" + id))
	disco := tailcat.DiscoPublicForNode(k).AppendTo(nil)
	b := NetworkBinding{AccountID: "account_test", EndpointID: id, Role: role, EndpointGeneration: 1, KeyGeneration: 1, WireGuardPublicKey: base64.RawURLEncoding.EncodeToString(p[:]), DiscoPublicKey: base64.RawURLEncoding.EncodeToString(disco), QUICCertificateFingerprint: hex.EncodeToString(fp[:]), QUICPublicKey: base64.RawURLEncoding.EncodeToString(fp[:]), VirtualAddress: addr}
	if role == "machine" {
		b.MachineID = id
		b.MachineGeneration = 1
	}
	return b
}
func networkTestAuthority(t *testing.T) (*Authority, NetworkConfiguration, ed25519.PrivateKey, *networkTestStore) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := key.NewNode(), key.NewNode()
	self := networkTestBinding("cli", "cli_test", "fd7a:115c:a1e0::1", local)
	peer := networkTestBinding("machine", "machine_test", "fd7a:115c:a1e0::2", remote)
	secrets := &networkTestStore{values: map[string]string{}}
	a, err := NewAuthority(AuthorityOptions{Store: config.ProfileStore{Path: t.TempDir(), Secrets: secrets}, Issuer: "https://api.example.test", Self: self, Keys: networkTestKeys{"network_test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	err = a.state(func(s *config.PeerNetworkState) error {
		raw := local.Raw32()
		s.PendingKey = append([]byte(nil), raw[:]...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	cfg := NetworkConfiguration{Version: 1, Issuer: a.options.Issuer, Audience: "paperboat-network", IssuedAt: now, ExpiresAt: now + 300, Generation: 1, Self: self, Peers: []NetworkPeer{{Identity: peer, Scopes: []NetworkScope{{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "terminal", Direction: "dial", Port: 443, ExpiresAt: now + 300}}}}}
	return a, cfg, priv, secrets
}

func TestNetworkAuthorityRejectsInvalidBindingsAndScopes(t *testing.T) {
	cases := map[string]func(*NetworkConfiguration){
		"wrong_account": func(c *NetworkConfiguration) { c.Self.AccountID = "other" },
		"wrong_role":    func(c *NetworkConfiguration) { c.Self.Role = "machine" },
		"wrong_certificate": func(c *NetworkConfiguration) {
			c.Self.QUICCertificateFingerprint = hex.EncodeToString(make([]byte, 32))
		},
		"wrong_key": func(c *NetworkConfiguration) {
			p := key.NewNode().Public().Raw32()
			c.Self.WireGuardPublicKey = base64.RawURLEncoding.EncodeToString(p[:])
		},
		"ungranted":               func(c *NetworkConfiguration) { c.Peers[0].Scopes = nil },
		"wrong_direction":         func(c *NetworkConfiguration) { c.Peers[0].Scopes[0].Direction = "accept" },
		"wrong_port":              func(c *NetworkConfiguration) { c.Peers[0].Scopes[0].Port = 22 },
		"unknown_scope":           func(c *NetworkConfiguration) { c.Peers[0].Scopes[0].Capability = "execute_everything" },
		"codex_on_machine_access": func(c *NetworkConfiguration) { c.Peers[0].Scopes[0].Capability = "codex" },
		"terminal_on_codex_session": func(c *NetworkConfiguration) {
			c.Peers[0].Scopes[0].ResourceKind = "codex_session"
		},
		"expired":           func(c *NetworkConfiguration) { c.IssuedAt = time.Now().Unix() - 2; c.ExpiresAt = time.Now().Unix() - 1 },
		"future":            func(c *NetworkConfiguration) { c.IssuedAt += 1; c.ExpiresAt += 1 },
		"scope_expired":     func(c *NetworkConfiguration) { c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt - 1 },
		"address_collision": func(c *NetworkConfiguration) { c.Peers[0].Identity.VirtualAddress = c.Self.VirtualAddress },
		"oversized_peers": func(c *NetworkConfiguration) {
			for len(c.Peers) <= MaxFlows {
				c.Peers = append(c.Peers, c.Peers[0])
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a, c, priv, _ := networkTestAuthority(t)
			mutate(&c)
			if err := a.Apply(context.Background(), networkToken(t, priv, c)); err == nil {
				t.Fatal("invalid authority accepted")
			}
			if a.Allows("machine_test", "grant_test", "terminal", "dial") {
				t.Fatal("invalid authority installed")
			}
		})
	}
	a, c, priv, _ := networkTestAuthority(t)
	token := networkToken(t, priv, c)
	token = token[:len(token)-4] + "AAAA"
	if a.Apply(context.Background(), token) == nil {
		t.Fatal("invalid signature accepted")
	}
}

func TestNetworkAuthorityAcceptsSignedCrossAccountPeer(t *testing.T) {
	a, cfg, private, _ := networkTestAuthority(t)
	cfg.Peers[0].Identity.AccountID = "shared_machine_owner"
	if err := a.Apply(t.Context(), networkToken(t, private, cfg)); err != nil {
		t.Fatal(err)
	}
	if !a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("signed cross-account machine scope was not installed")
	}
}

func TestNetworkAuthorityAcceptsExactCodexSessionScope(t *testing.T) {
	a, configuration, private, _ := networkTestAuthority(t)
	configuration.Peers[0].Scopes[0] = NetworkScope{ResourceKind: "codex_session", ResourceID: "cdx_test", ResourceGeneration: 1, Capability: "codex", Direction: "dial", Port: NetworkPort, ExpiresAt: configuration.ExpiresAt}
	if err := a.Apply(context.Background(), networkToken(t, private, configuration)); err != nil {
		t.Fatal(err)
	}
	if !a.Allows("machine_test", "cdx_test", "codex", "dial") || a.Allows("machine_test", "cdx_test", "terminal", "dial") {
		t.Fatal("Codex scope was not kept separate from terminal authority")
	}
}

func TestNetworkAuthorityRotationReplayRestartAndFailedWrite(t *testing.T) {
	a, c, priv, secrets := networkTestAuthority(t)
	ctx := context.Background()
	token := networkToken(t, priv, c)
	if err := a.Apply(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(ctx, token); err != nil {
		t.Fatal("identical replay rejected:", err)
	}
	changed := c
	changed.Peers = nil
	if !errors.Is(a.Apply(ctx, networkToken(t, priv, changed)), ErrStaleAuthority) {
		t.Fatal("same generation change accepted")
	}
	if err := a.Apply(ctx, token); err != nil {
		t.Fatal(err)
	}
	public, generation, err := a.PrepareKey(true)
	if err != nil || generation != 1 {
		t.Fatal("prepare rotation failed")
	}
	publicAgain, _, err := a.PrepareKey(true)
	if err != nil || publicAgain != public {
		t.Fatal("pending key lost")
	}
	c.Generation++
	c.Self.KeyGeneration++
	c.Self.WireGuardPublicKey = public
	secrets.fail = true
	if a.Apply(ctx, networkToken(t, priv, c)) == nil {
		t.Fatal("failed durable write installed authority")
	}
	if a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("traffic admitted after storage failure")
	}
	secrets.fail = false
	if err := a.Apply(ctx, networkToken(t, priv, c)); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAuthority(a.options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !errors.Is(restarted.Apply(ctx, token), ErrStaleAuthority) {
		t.Fatal("restart revived old authority")
	}
	if err := restarted.Apply(ctx, networkToken(t, priv, c)); err != nil {
		t.Fatal(err)
	}
	c.Generation++
	c.Peers = nil
	if err := restarted.Apply(ctx, networkToken(t, priv, c)); err != nil {
		t.Fatal(err)
	}
	if restarted.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("empty replacement retained peer")
	}
}

func TestNetworkAuthorityPartitionExpiry(t *testing.T) {
	a, c, priv, _ := networkTestAuthority(t)
	c.ExpiresAt = time.Now().Unix() + 1
	if err := a.Apply(context.Background(), networkToken(t, priv, c)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.Allows("machine_test", "grant_test", "terminal", "dial") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("partition extended authority")
	}
	if !errors.Is(a.Apply(context.Background(), networkToken(t, priv, c)), ErrExpiredAuthority) {
		t.Fatal("expired replay accepted")
	}
}

func TestNetworkAuthorityExpiryDoesNotWaitForStorage(t *testing.T) {
	a, c, signer, store := networkTestAuthority(t)
	c.ExpiresAt = time.Now().Unix() + 1
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	store.onSet = func() { close(started); <-release }
	c.Generation++
	token := networkToken(t, signer, c)
	finished := make(chan error, 1)
	go func() { finished <- a.Apply(t.Context(), token) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("storage mutation not started")
	}
	time.Sleep(time.Until(time.Unix(c.ExpiresAt, 0)) + 20*time.Millisecond)
	checked := make(chan bool, 1)
	go func() { checked <- a.Allows("machine_test", "grant_test", "terminal", "dial") }()
	select {
	case allowed := <-checked:
		if allowed {
			t.Fatal("stalled storage extended authority")
		}
	case <-time.After(time.Second):
		t.Fatal("expiry blocked behind storage")
	}
	close(release)
	select {
	case err := <-finished:
		if !errors.Is(err, ErrExpiredAuthority) {
			t.Fatal("expired late installation was not rejected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("storage completion did not release apply")
	}
}

func TestNetworkAllocatedUDPRotationRemovalAndExpiry(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	cli, c, signer, _ := networkTestAuthority(t)
	machineKey := key.NewNode()
	machineBinding := networkTestBinding("machine", "machine_test", "fd7a:115c:a1e0::2", machineKey)
	c.Peers[0].Identity = machineBinding
	machine, err := NewAuthority(AuthorityOptions{Store: config.ProfileStore{Path: t.TempDir(), Secrets: &networkTestStore{values: map[string]string{}}}, Issuer: c.Issuer, Self: machineBinding, Keys: cli.options.Keys})
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	if err := machine.state(func(s *config.PeerNetworkState) error {
		k := machineKey.Raw32()
		s.PendingKey = append([]byte(nil), k[:]...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m := c
	m.Self = machineBinding
	m.Peers = []NetworkPeer{{Identity: c.Self, Scopes: append([]NetworkScope(nil), c.Peers[0].Scopes...)}}
	m.Peers[0].Scopes[0].Direction = "accept"
	apply := func(a *Authority, c NetworkConfiguration) {
		t.Helper()
		if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
			t.Fatal(err)
		}
	}
	apply(cli, c)
	apply(machine, m)
	server, err := machine.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	exchange := func() (*Packet, *Packet) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		client, err := cli.Client(server.Address(), machineBinding.EndpointID)
		if err != nil {
			t.Fatal(err)
		}
		out, err := client.Dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = out.Close() })
		if _, err := out.Write([]byte("authority")); err != nil {
			t.Fatal(err)
		}
		in, err := server.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = in.Close() })
		_ = in.SetDeadline(time.Now().Add(3 * time.Second))
		_ = out.SetDeadline(time.Now().Add(3 * time.Second))
		var buf [32]byte
		n, err := in.Read(buf[:])
		if err != nil || string(buf[:n]) != "authority" {
			t.Fatal("allocated payload not received", err)
		}
		if in.LocalAddr().String() != "[fd7a:115c:a1e0::2]:443" {
			t.Fatal("server used key-derived address")
		}
		if _, err := in.Write([]byte("verified")); err != nil {
			t.Fatal(err)
		}
		n, err = out.Read(buf[:])
		if err != nil || string(buf[:n]) != "verified" {
			t.Fatal("allocated reply not received", err)
		}
		return out, in
	}
	out, in := exchange()
	// Identical refresh must preserve active traffic.
	c.Generation++
	m.Generation++
	apply(cli, c)
	apply(machine, m)
	if _, err := out.Write([]byte("refresh")); err != nil {
		t.Fatal("refresh closed client", err)
	}
	var buf [32]byte
	if _, err := in.Read(buf[:]); err != nil {
		t.Fatal("refresh closed server flow", err)
	}
	// Transport restart retains authority but reacquires usable engine leases.
	cli.InvalidateClient(cli.clients[machineBinding.EndpointID].client)
	out, in = exchange()
	_ = server.Close()
	server, err = machine.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	// Native QUIC owns failure detection. Once a remote engine has failed, its
	// exact cached UDP client is discarded before the bounded reconnect.
	cli.InvalidateClient(cli.clients[machineBinding.EndpointID].client)
	out, in = exchange()
	// Revoke both views. An already-held lease cannot keep exchanging payloads.
	cPeers, mPeers := c.Peers, m.Peers
	c.Generation++
	m.Generation++
	c.Peers = nil
	m.Peers = nil
	apply(cli, c)
	apply(machine, m)
	if _, err := in.Write([]byte("revoked")); err == nil {
		t.Fatal("revoked server lease stayed writable")
	}
	if _, err := out.Write([]byte("revoked")); err == nil {
		t.Fatal("revoked client lease stayed writable")
	}
	// Restore with a rotated key at the same allocated address.
	public, _, err := cli.PrepareKey(true)
	if err != nil {
		t.Fatal(err)
	}
	c.Generation++
	m.Generation++
	c.Peers = cPeers
	m.Peers = mPeers
	c.Self.KeyGeneration++
	c.Self.WireGuardPublicKey = public
	m.Peers[0].Identity = c.Self
	apply(machine, m)
	apply(cli, c)
	_, in = exchange()
	m.Generation++
	m.ExpiresAt = time.Now().Unix() + 1
	apply(machine, m)
	time.Sleep(time.Until(time.Unix(m.ExpiresAt, 0)) + 20*time.Millisecond)
	if _, err := in.Write([]byte("expired")); err == nil {
		t.Fatal("partition retained expired socket")
	}
}

func TestNetworkDescriptorUsesSignedPeerAndAdmittedRegion(t *testing.T) {
	authority, configuration, signer, _ := networkTestAuthority(t)
	defer authority.Close()
	if err := authority.Apply(t.Context(), networkToken(t, signer, configuration)); err != nil {
		t.Fatal(err)
	}
	authority.relay.mu.Lock()
	authority.relay.nodes = map[tailcfg.DERPRegionID]RegionalNode{
		regionalID("relay_descriptor"): {NodeID: "relay_descriptor", NodeGeneration: 1, ProcessEpoch: "epoch_descriptor", Region: "test", EndpointHost: "relay.example.test", EndpointQUICPort: 443},
	}
	authority.relay.mu.Unlock()
	descriptor, err := authority.Descriptor(configuration.Peers[0].Identity.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := tailcat.ParseAddr(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	peerKey, _ := publicKey(configuration.Peers[0].Identity.WireGuardPublicKey)
	disco, _ := derpquic.ParseDiscoKey(configuration.Peers[0].Identity.DiscoPublicKey)
	if parsed.ServerPublic.NodePublic != peerKey || parsed.ServerDiscoPublic.DiscoPublic != disco || !parsed.PresharedKey.IsZero() || len(parsed.Region) != 1 || parsed.Region[0].Nodes[0].HostName != "relay.example.test" {
		t.Fatalf("descriptor did not preserve signed peer and admitted region: %+v", parsed)
	}
}

// This test consumes signatures produced by the real SQL-backed server test.
func TestNetworkProducedConfiguration(t *testing.T) {
	path := os.Getenv("PAPERBOAT_NETWORK_FIXTURE")
	if path == "" {
		t.Skip("requires SQL producer fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Issuer            string         `json:"issuer"`
		KeyID             string         `json:"signing_key_id"`
		Public            string         `json:"signing_public_key"`
		CLI               NetworkBinding `json:"cli"`
		Machine           NetworkBinding `json:"machine"`
		CLIPrivate        string         `json:"cli_private_key"`
		MachinePrivate    string         `json:"machine_private_key"`
		CLIConfig         string         `json:"cli_configuration"`
		MachineConfig     string         `json:"machine_configuration"`
		Revoked           string         `json:"revoked_cli_configuration"`
		InitialCLIConfig  string         `json:"initial_cli_configuration"`
		InitialCLIPrivate string         `json:"initial_cli_private_key"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	pub, err := base64.RawURLEncoding.DecodeString(f.Public)
	if err != nil {
		t.Fatal(err)
	}
	create := func(self NetworkBinding, private, token string) *Authority {
		t.Helper()
		root := t.TempDir()
		a, err := NewAuthority(AuthorityOptions{Store: config.ProfileStore{Path: root, Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}}, Issuer: f.Issuer, Self: self, Keys: networkTestKeys{f.KeyID: ed25519.PublicKey(pub)}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		k, err := base64.RawURLEncoding.DecodeString(private)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(k)
		if err := a.state(func(s *config.PeerNetworkState) error { s.PendingKey = append([]byte(nil), k...); return nil }); err != nil {
			t.Fatal(err)
		}
		if err := a.Apply(context.Background(), token); err != nil {
			t.Fatal("actual server config rejected:", err)
		}
		return a
	}
	cli := create(f.CLI, f.InitialCLIPrivate, f.InitialCLIConfig)
	rotated, err := base64.RawURLEncoding.DecodeString(f.CLIPrivate)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(rotated)
	if err := cli.state(func(s *config.PeerNetworkState) error { s.PendingKey = append([]byte(nil), rotated...); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := cli.Apply(t.Context(), f.CLIConfig); err != nil {
		t.Fatal("actual rotated configuration rejected:", err)
	}
	_ = create(f.Machine, f.MachinePrivate, f.MachineConfig)
	if len(cli.current.Peers) == 0 {
		t.Fatal("producer supplied no granted peer")
	}
	if err := cli.Apply(context.Background(), f.Revoked); err != nil {
		t.Fatal("actual revocation rejected:", err)
	}
	if len(cli.current.Peers) != 0 {
		t.Fatal("actual revoked grant retained")
	}
}
