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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
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
func (n networkAPIFunc) PeerNetworkConfiguration(ctx context.Context, op string, _ []api.AdmittedNetworkScope) (api.PeerNetworkConfigurationResult, error) {
	token, err := n.configuration(ctx, op)
	return api.PeerNetworkConfigurationResult{Configuration: token}, err
}

func TestNetworkRegistrationLostResponseAndRefreshRecovery(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	c.ExpiresAt = c.IssuedAt + int64((7*time.Minute)/time.Second)
	calls := 0
	var previous api.PeerNetworkRegistration
	client := networkAPIFunc{
		register: func(_ context.Context, r api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
			calls++
			if calls == 2 && r != previous {
				t.Error("interrupted registration did not replay exact operation")
			}
			previous = r
			if c.Self.WireGuardPublicKey != r.WireGuardPublicKey {
				c.Self.KeyGeneration = r.ExpectedKeyGeneration + 1
			}
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
	if calls != 3 || c.Self.KeyGeneration != 1 {
		t.Fatal("ordinary registration changed a healthy network key")
	}
	if err := a.Register(t.Context(), client, true); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || c.Self.KeyGeneration != 2 {
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

func TestLiveNativeScopeObservesWithdrawalBeforeCredentialRefresh(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	release, err := a.TrackAdmittedScope("machine_test", "grant_test", "terminal", "dial")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var withdrawn atomic.Bool
	first := make(chan struct{}, 1)
	client := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
		c.Generation++
		if withdrawn.Load() {
			c.Peers = nil
		}
		select {
		case first <- struct{}{}:
		default:
		}
		return networkToken(t, signer, c), nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	select {
	case <-first:
	case <-time.After(40 * time.Second):
		t.Fatal("initial network refresh unavailable")
	}
	withdrawn.Store(true)
	deadline := time.Now().Add(2500 * time.Millisecond)
	for a.AllowsAdmitted("machine_test", "grant_test", "terminal", "dial") {
		if time.Now().After(deadline) {
			t.Fatal("live operation waited for credential refresh to observe withdrawal")
		}
		time.Sleep(time.Millisecond)
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
func networkTokenWithRetiredRelayPairs(t *testing.T, priv ed25519.PrivateKey, c NetworkConfiguration) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-network-config+jwt", "kid": "network_test"})
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatal(err)
	}
	claims["relay_pairs"] = []any{}
	body, err = json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	data := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return data + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(data)))
}
func networkTestBinding(role, id, addr string, k key.NodePrivate) NetworkBinding {
	p := k.Public().Raw32()
	fp := sha256.Sum256([]byte("quic:" + id))
	disco := mesh.DiscoPublicForNode(k).AppendTo(nil)
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
		"expired": func(c *NetworkConfiguration) { c.IssuedAt = time.Now().Unix() - 2; c.ExpiresAt = time.Now().Unix() - 1 },
		"future": func(c *NetworkConfiguration) {
			c.IssuedAt = time.Now().Add(derpquic.MaxClockSkew + time.Second).Unix()
			c.ExpiresAt = c.IssuedAt + 300
		},
		"missing_scope_expiry": func(c *NetworkConfiguration) { c.Peers[0].Scopes[0].ExpiresAt = 0 },
		"scope_expired":        func(c *NetworkConfiguration) { c.Peers[0].Scopes[0].ExpiresAt = c.IssuedAt },
		"address_collision":    func(c *NetworkConfiguration) { c.Peers[0].Identity.VirtualAddress = c.Self.VirtualAddress },
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
			if name == "scope_expired" {
				if err := a.Apply(t.Context(), networkToken(t, priv, c)); err != nil {
					t.Fatal("retained signed scope rejected", err)
				}
				if a.Allows("machine_test", "grant_test", "terminal", "dial") {
					t.Fatal("expired scope admitted new operation")
				}
				if release, err := a.TrackAdmittedScope("machine_test", "grant_test", "terminal", "dial"); err == nil {
					release()
					t.Fatal("expired scope gained a new live-operation ref")
				}
				return
			}
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

func TestNetworkAuthorityRejectsRetiredMachineRelayClaims(t *testing.T) {
	a, c, priv, _ := networkTestAuthority(t)
	if err := a.Apply(context.Background(), networkTokenWithRetiredRelayPairs(t, priv, c)); err == nil {
		t.Fatal("retired enrolled-machine relay claim was accepted")
	}
	if a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("retired relay claim installed network authority")
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

func TestNetworkAdmissionDoesNotAuthorizeApplicationStreams(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: cfg.ExpiresAt}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"grant_test", "machine_test", "", "*"} {
		for _, capability := range []string{"terminal", "exec", "private_access", "file_transfer", "managed_ssh", "config_compare"} {
			if a.Allows("machine_test", resource, capability, "dial") {
				t.Fatalf("network admission authorized %s / %s", resource, capability)
			}
		}
	}
	cfg.Generation++
	cfg.Peers[0].Identity.AccountID = "another_account"
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err == nil {
		t.Fatal("cross-account personal network admission accepted")
	}
}

func TestExactApplicationScopeRejectsWildcardRequests(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"machine_test", "", "*", "other_grant"} {
		if a.Allows("machine_test", resource, "terminal", "dial") {
			t.Fatalf("ungranted resource %q accepted", resource)
		}
	}
}

func TestPersonalMachinePeersHaveOnlyNetworkAdmission(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: cfg.ExpiresAt}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal("personal machine network admission rejected:", err)
	}
	cfg.Generation++
	cfg.Peers[0].Scopes[0].ResourceKind = "machine_access"
	cfg.Peers[0].Scopes[0].Capability = "terminal"
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err == nil {
		t.Fatal("machine-to-machine network admission gained a terminal scope")
	}
}

func TestReorderedNetworkAuthorityKeepsCurrentButConflictingGenerationFailsClosed(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Generation = 2
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	older := cfg
	older.Generation = 1
	if err := a.Apply(t.Context(), networkToken(t, signer, older)); !errors.Is(err, ErrStaleAuthority) || !a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatalf("reordered authority retired valid grants: %v", err)
	}
	cfg.Peers[0].Scopes[0].ResourceID = "conflicting_grant"
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); !errors.Is(err, ErrAuthority) || a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatalf("conflicting generation did not fail closed: %v", err)
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
	if !errors.Is(a.Apply(ctx, networkToken(t, priv, changed)), ErrAuthority) {
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

func TestNetworkAllocatedUDPRotationRemovalAndAdmittedLifetime(t *testing.T) {
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
	liveClient, err := cli.Client(server.Address(), machineBinding.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	if cli.RetireIdleClient(liveClient) {
		t.Fatal("failed extra dial retired an admitted UDP flow")
	}
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
	// Released transport retains authority but reacquires usable engine leases.
	idleClient, err := cli.Client(server.Address(), machineBinding.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	_ = in.Close()
	if !cli.RetireIdleClient(idleClient) {
		t.Fatal("released UDP client could not retire its stale route")
	}
	out, in = exchange()
	if cli.RetireIdleClient(idleClient) {
		t.Fatal("late transport failure retired a replacement client")
	}
	_ = server.Close()
	server, err = machine.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	// Native QUIC releases its failed packet before retiring the cached route.
	restartedClient, err := cli.Client(server.Address(), machineBinding.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	_ = in.Close()
	if !cli.RetireIdleClient(restartedClient) {
		t.Fatal("failed idle engine route was retained")
	}
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
	// The snapshot expires for new admission, while the admitted packet flow remains usable.
	time.Sleep(time.Until(time.Unix(m.ExpiresAt, 0)) + 20*time.Millisecond)
	if _, err := in.Write([]byte("admitted")); err != nil {
		t.Fatal("credential expiry closed admitted socket", err)
	}
	m.Generation++
	m.IssuedAt = time.Now().Unix()
	m.ExpiresAt = m.IssuedAt + 60
	m.Peers = nil
	apply(machine, m)
	if _, err := in.Write([]byte("revoked")); err == nil {
		t.Fatal("revocation retained admitted socket")
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
	parsed, err := mesh.ParseAddr(descriptor)
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

func TestNetworkIssuedAtToleratesClockSkewWithoutExtendingExpiry(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	token := networkToken(t, signer, c)
	for _, seconds := range []int64{1, 3, 60} {
		if _, _, err := a.verify(t.Context(), token, time.Unix(c.IssuedAt-seconds, 0)); err != nil {
			t.Fatalf("%ds clock skew: %v", seconds, err)
		}
	}
	if _, _, err := a.verify(t.Context(), token, time.Unix(c.IssuedAt-61, 0)); err == nil {
		t.Fatal("excessive clock skew accepted")
	}
	if _, _, err := a.verify(t.Context(), token, time.Unix(c.ExpiresAt, 0)); err == nil {
		t.Fatal("expired authority accepted")
	}
}

func TestConfigCompareAdmissionIsExactAndCannotOpenTerminal(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "config_compare", ResourceID: "assignment_1", ResourceGeneration: 1, Capability: "config_compare", Direction: "dial", Port: 443, ExpiresAt: cfg.ExpiresAt}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	if !a.Allows("machine_test", "assignment_1", "config_compare", "dial") {
		t.Fatal("exact compare admission rejected")
	}
	if a.Allows("machine_test", "assignment_2", "config_compare", "dial") || a.Allows("machine_test", "assignment_1", "terminal", "dial") {
		t.Fatal("compare admission expanded authority")
	}
}

func TestNetworkRunRenewsShortSignedValidityAndRecoversAfterExpiry(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	var failing atomic.Bool
	stalled := make(chan struct{}, 1)
	resume := make(chan struct{})
	renewed := make(chan int64, 32)
	client := networkAPIFunc{configuration: func(ctx context.Context, _ string) (string, error) {
		if failing.Load() {
			select {
			case stalled <- struct{}{}:
			default:
			}
			select {
			case <-resume:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "", &api.APIError{Status: 503, Code: "peer_network_unavailable"}
		}
		c.Generation++
		c.IssuedAt = time.Now().Unix()
		c.ExpiresAt = c.IssuedAt + 3
		c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt + 60
		token := networkToken(t, signer, c)
		renewed <- c.ExpiresAt
		return token, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	await := func() int64 {
		t.Helper()
		select {
		case expiry := <-renewed:
			return expiry
		case <-time.After(5 * time.Second):
			t.Fatal("short-lived authority was not renewed")
			return 0
		}
	}
	first := await()
	for !a.Allows("machine_test", "grant_test", "terminal", "dial") {
		time.Sleep(time.Millisecond)
	}
	descriptor, err := a.Descriptor("machine_test")
	if err != nil {
		t.Fatal(err)
	}
	attached, err := a.Client(descriptor, "machine_test")
	if err != nil {
		t.Fatal(err)
	}
	second := await()
	if time.Now().Unix() >= first || second <= first || !a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("renewal did not preserve valid authority before expiry")
	}
	again, err := a.Client(descriptor, "machine_test")
	if err != nil || again != attached {
		t.Fatal("valid renewal replaced the attached native client", err)
	}
	failing.Store(true)
	deadline := time.NewTimer(time.Until(time.Unix(second, 0)) + 500*time.Millisecond)
	defer deadline.Stop()
	<-deadline.C
	if a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("failed renewal extended signed authority")
	}
	if _, err := a.Descriptor("machine_test"); !errors.Is(err, ErrAuthority) || !errors.Is(err, ErrExpiredAuthority) {
		t.Fatalf("expired authority lost typed lifetime cause: %T", err)
	}
	select {
	case <-stalled:
	default:
		t.Fatal("refresh did not stall across expiry")
	}
	failing.Store(false)
	close(resume)
	await()
	limit := time.Now().Add(time.Second)
	for !a.Allows("machine_test", "grant_test", "terminal", "dial") && time.Now().Before(limit) {
		time.Sleep(10 * time.Millisecond)
	}
	if !a.Allows("machine_test", "grant_test", "terminal", "dial") {
		t.Fatal("fresh signed authority did not recover")
	}
}

func TestNetworkScopeExpiryRejectsNewWorkAndPreservesAdmittedClient(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	short := time.Now().Unix() + 2
	c.Peers[0].Scopes[0].ExpiresAt = short
	live := c.Peers[0].Scopes[0]
	live.ResourceID = "live_files"
	live.Capability = "file_transfer"
	live.ExpiresAt = c.ExpiresAt
	c.Peers[0].Scopes = append(c.Peers[0].Scopes, live)
	only := networkTestBinding("machine", "machine_expiring", "fd7a:115c:a1e0::3", key.NewNode())
	c.Peers = append(c.Peers, NetworkPeer{Identity: only, Scopes: []NetworkScope{c.Peers[0].Scopes[0]}})
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	descriptor, err := a.Descriptor("machine_test")
	if err != nil {
		t.Fatal(err)
	}
	attached, err := a.Client(descriptor, "machine_test")
	if err != nil {
		t.Fatal(err)
	}
	onlyDescriptor, err := a.Descriptor("machine_expiring")
	if err != nil {
		t.Fatal(err)
	}
	expiredClient, err := a.Client(onlyDescriptor, "machine_expiring")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(time.Unix(short, 0)))
	withdrawalDeadline := time.Now().Add(time.Second)
	for {
		if _, err := a.Peer("machine_expiring"); errors.Is(err, ErrAdmission) {
			break
		}
		if time.Now().After(withdrawalDeadline) {
			t.Fatal("expired-only peer was not withdrawn")
		}
		time.Sleep(time.Millisecond)
	}
	if a.Allows("machine_test", "grant_test", "terminal", "dial") || !a.Allows("machine_test", "live_files", "file_transfer", "dial") {
		t.Fatal("resource expiry widened or discarded unrelated authority")
	}
	again, err := a.Client(descriptor, "machine_test")
	if err != nil || again != attached {
		t.Fatal("expired unrelated scope replaced live native client", err)
	}
	if _, err := a.Descriptor("machine_expiring"); !errors.Is(err, ErrAdmission) {
		t.Fatal("expired-only peer retained admission", err)
	}
	probeCtx, cancelProbe := context.WithTimeout(t.Context(), time.Second)
	defer cancelProbe()
	if _, err := expiredClient.Dial(probeCtx); !errors.Is(err, net.ErrClosed) {
		t.Fatal("expired-only native client remained usable", err)
	}
	c.Generation++
	c.Peers = c.Peers[:1]
	c.Peers[0].Scopes[1].ExpiresAt = live.ExpiresAt - 1
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	again, err = a.Client(descriptor, "machine_test")
	if err != nil || again != attached {
		t.Fatal("credential lifetime change replaced admitted client", err)
	}
	c.Generation++
	c.Peers = nil
	if err := a.Apply(t.Context(), networkToken(t, signer, c)); err != nil {
		t.Fatal(err)
	}
	if _, err := attached.Dial(probeCtx); !errors.Is(err, net.ErrClosed) {
		t.Fatal("revoked client admitted work", err)
	}

}

func TestNetworkReceiptPrunesExpiredScopesAfterSignedValidation(t *testing.T) {
	for _, invalid := range []string{"", "expired_invalid_shape", "expired_scope_overflow"} {
		t.Run(invalid, func(t *testing.T) {
			a, c, signer, _ := networkTestAuthority(t)
			now := time.Now().Unix()
			c.IssuedAt = now - 5
			c.ExpiresAt = c.IssuedAt + 300
			old := c.Peers[0].Scopes[0]
			old.ExpiresAt = now - 1
			live := old
			live.ResourceID = "live_files"
			live.Capability = "file_transfer"
			live.ExpiresAt = c.ExpiresAt
			c.Peers[0].Scopes = []NetworkScope{old, live}
			only := networkTestBinding("machine", "machine_expired", "fd7a:115c:a1e0::3", key.NewNode())
			c.Peers = append(c.Peers, NetworkPeer{Identity: only, Scopes: []NetworkScope{old}})
			if invalid == "expired_invalid_shape" {
				c.Peers[1].Scopes[0].Port = 22
			}
			if invalid == "expired_scope_overflow" {
				for i := 0; i < 129; i++ {
					scope := old
					scope.ResourceID = fmt.Sprintf("old_%d", i)
					c.Peers[1].Scopes = append(c.Peers[1].Scopes, scope)
				}
			}
			token := networkToken(t, signer, c)
			err := a.Apply(t.Context(), token)
			if invalid != "" {
				if !errors.Is(err, ErrAuthority) {
					t.Fatal("expired malformed/bounded scopes bypassed validation", err)
				}
				return
			}
			if err != nil {
				t.Fatal("valid delayed configuration rejected", err)
			}
			if a.Allows("machine_test", "grant_test", "terminal", "dial") || !a.Allows("machine_test", "live_files", "file_transfer", "dial") {
				t.Fatal("delayed scope projection lost resource fences")
			}
			if _, err := a.Peer("machine_expired"); !errors.Is(err, ErrAdmission) {
				t.Fatal("expired-only peer retained admission", err)
			}
			descriptor, err := a.Descriptor("machine_test")
			if err != nil {
				t.Fatal(err)
			}
			attached, err := a.Client(descriptor, "machine_test")
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Apply(t.Context(), token); err != nil {
				t.Fatal("identical signed retry changed generation fence", err)
			}
			again, err := a.Client(descriptor, "machine_test")
			if err != nil || again != attached {
				t.Fatal("valid delayed retry replaced live native client", err)
			}
		})
	}
}
