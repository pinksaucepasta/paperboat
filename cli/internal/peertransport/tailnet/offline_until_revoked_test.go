package tailnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"strconv"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"tailscale.com/types/key"
)

// F21-01: Paperboat unreachable for 10 minutes past the configuration's refresh hint. The daemon keeps its last
// verified peers (machine_network scopes without expiry), retries with the failure backoff instead of every
// 100 ms, and applies a revocation (a newer generation without the peer) as soon as it reconnects; a replayed
// lower generation stays refused.
func TestNetworkOutageKeepsPeersUntilARevocationArrives(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.ExpiresAt = time.Now().Unix() + 2
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: 0}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal("until-revoked network scope rejected:", err)
	}
	time.Sleep(time.Until(time.Unix(cfg.ExpiresAt, 0)) + 50*time.Millisecond)

	// The outage: every refresh fails. Ten minutes of the real schedule, from the delays Run would wait.
	requests, elapsed := 0, time.Duration(0)
	for failures := 1; elapsed < 10*time.Minute; failures++ {
		requests++
		elapsed += a.nextRefresh(errors.New("paperboat unreachable"), failures, 0)
	}
	// The failure backoff is capped by the healthy cadence (24-36 s), so an outage costs no more than a healthy
	// engine: at most 10 min / 24 s plus the first backoff steps. The 100 ms floor would be ~6,000.
	if limit := int(10*time.Minute/(24*time.Second)) + 7; requests > limit {
		t.Fatalf("%d refresh requests in a 10-minute outage, want at most %d", requests, limit)
	}
	if _, err := a.Peer("machine_test"); err != nil {
		t.Fatal("the outage dropped a verified peer:", err)
	}
	if !a.OutOfSync() {
		t.Fatal("the outage is not reported out of sync")
	}

	// Reconnect: the server's newer configuration no longer has the peer (revoked during the outage).
	revoked := cfg
	revoked.Generation++
	revoked.IssuedAt, revoked.ExpiresAt = time.Now().Unix(), time.Now().Unix()+120
	revoked.Peers = nil
	if err := a.Apply(t.Context(), networkToken(t, signer, revoked)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Peer("machine_test"); !errors.Is(err, ErrAdmission) {
		t.Fatal("a revocation delivered after reconnect was not applied:", err)
	}
	if a.OutOfSync() {
		t.Fatal("still out of sync after a fresh configuration")
	}
	stale := cfg
	stale.IssuedAt, stale.ExpiresAt = time.Now().Unix(), time.Now().Unix()+120
	if err := a.Apply(context.Background(), networkToken(t, signer, stale)); !errors.Is(err, ErrStaleAuthority) {
		t.Fatalf("a lower generation after the revocation = %v, want refused as stale", err)
	}
}

// Only machine_network scopes may be until-revoked: a grant without expiry is refused.
func TestOnlyNetworkScopesMayLastUntilRevoked(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Peers[0].Scopes[0].ExpiresAt = 0
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); !errors.Is(err, ErrAuthority) {
		t.Fatalf("a machine_access grant without expiry = %v, want refused", err)
	}
}

// F21-11: a host's offline session rights are machine_access accept scopes without expiry, at the deciding
// policy version; Allows answers for them past the refresh hint.
func TestHostSessionRightsLastUntilRevoked(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.Peers[0].Identity = networkTestBinding("cli", "cli_peer", "fd7a:115c:a1e0::3", key.NewNode())
	cfg.ExpiresAt = time.Now().Unix() + 2
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_access", ResourceID: "host_test", ResourceGeneration: 7, Capability: "terminal", Direction: "accept", Port: 443, ExpiresAt: 0}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal("until-revoked session right rejected:", err)
	}
	time.Sleep(time.Until(time.Unix(cfg.ExpiresAt, 0)) + 50*time.Millisecond)
	if !a.Allows("cli_peer", "host_test", "terminal", "accept") {
		t.Fatal("an until-revoked session right ended at the refresh hint")
	}
	if a.Allows("cli_peer", "host_test", "exec", "accept") {
		t.Fatal("a capability that was not granted is allowed")
	}
}

type unreachableKeys struct{}

func (unreachableKeys) Lookup(context.Context, string) (ed25519.PublicKey, bool, error) {
	return nil, false, errors.New("paperboat unreachable")
}

// F21-01: a daemon restarted while Paperboat is unreachable (signing keys unreachable too) restores the
// configuration it last verified and keeps its peers; an altered persisted file is refused.
func TestRestartWithoutPaperboatRestoresTheLastVerifiedPeers(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.ExpiresAt = time.Now().Unix() + 2
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: 0}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	time.Sleep(time.Until(time.Unix(cfg.ExpiresAt, 0)) + 50*time.Millisecond)

	restart := func() *Authority {
		t.Helper()
		next, err := NewAuthority(AuthorityOptions{Store: a.options.Store, Issuer: a.options.Issuer, Self: cfg.Self, Keys: unreachableKeys{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = next.Close() })
		return next
	}
	unreachable := networkAPIFunc{register: func(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
		return api.PeerNetworkRegistrationResult{}, errors.New("dial tcp: connection refused")
	}}
	restarted := restart()
	if err := restarted.RegisterOrRestore(t.Context(), unreachable, false); err != nil {
		t.Fatal("restart without Paperboat lost the verified configuration:", err)
	}
	if _, err := restarted.Peer("machine_test"); err != nil {
		t.Fatal("restart without Paperboat dropped the peer:", err)
	}
	if !restarted.OutOfSync() || restarted.LastVerifiedAt().Unix() != cfg.IssuedAt {
		t.Fatal("a restored configuration must report out of sync since it was issued")
	}

	path, err := a.options.Store.LoadPeerNetworkConfiguration(a.options.Issuer, cfg.Self.AccountID, cfg.Self.EndpointID, MaxConfigurationBytes)
	if err != nil || path == "" {
		t.Fatal("configuration was not persisted:", err)
	}
	altered := cfg
	altered.Peers = append(altered.Peers, NetworkPeer{Identity: networkTestBinding("machine", "machine_extra", "fd7a:115c:a1e0::9", key.NewNode()), Scopes: cfg.Peers[0].Scopes})
	if err := a.options.Store.SavePeerNetworkConfiguration(a.options.Issuer, cfg.Self.AccountID, cfg.Self.EndpointID, networkToken(t, signer, altered)); err != nil {
		t.Fatal(err)
	}
	if err := restart().Restore(t.Context()); !errors.Is(err, ErrAuthority) {
		t.Fatalf("an altered persisted configuration = %v, want refused", err)
	}
	refused := networkAPIFunc{register: func(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
		return api.PeerNetworkRegistrationResult{}, &api.APIError{Status: 403, Code: "machine_revoked"}
	}}
	if err := restart().RegisterOrRestore(t.Context(), refused, false); err == nil {
		t.Fatal("a machine Paperboat refused restored its old peers")
	}

}

// The daemon asks for until-revoked authority on every configuration request (server 2026.10.23.94+).
func TestConfigurationRequestsAskForUntilRevokedAuthority(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	var asked string
	client := networkAPIFunc{
		configuration: func(context.Context, string) (string, error) { return networkToken(t, signer, cfg), nil },
		requests:      func(request api.PeerNetworkConfigurationRequest) { asked = request.Authority },
	}
	if err := a.Refresh(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	if asked != "until_revoked_v1" {
		t.Fatalf("authority asked = %q, want until_revoked_v1", asked)
	}
}

// Prod 2026-10-27: an engine refused (429) before its first configuration retried every second forever, because
// the 1 s first-configuration cadence also capped the failure backoff (2 req/s per carrier install). Failures
// before the first configuration back off like any other.
func TestRefusalsBeforeTheFirstConfigurationBackOff(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	requests, elapsed := 0, time.Duration(0)
	for failures := 1; elapsed < 10*time.Minute; failures++ {
		requests++
		elapsed += a.nextRefresh(errors.New("429 rate limited"), failures, 0)
	}
	if limit := int(10*time.Minute/(24*time.Second)) + 7; requests > limit {
		t.Fatalf("%d refresh requests in 10 minutes of refusals before a first configuration, want at most %d", requests, limit)
	}
}

func TestRestartRestoresVerifiedNetworkBeforeContactingControl(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Self.Role, cfg.Self.MachineID, cfg.Self.MachineGeneration = "machine", cfg.Self.EndpointID, 1
	a.options.Self = cfg.Self
	cfg.Peers[0].Scopes = []NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443}}
	if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	options := a.options
	// Production knows endpoint identity, not the network fields assigned by the server.
	options.Self.VirtualAddress = ""
	options.Self.WireGuardPublicKey = ""
	options.Self.DiscoPublicKey = ""
	options.Self.KeyGeneration = 0
	restarted, err := NewAuthority(options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	requests := 0
	control := networkAPIFunc{register: func(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
		requests++
		return api.PeerNetworkRegistrationResult{}, context.DeadlineExceeded
	}}
	if err := restarted.RestoreOrRegister(t.Context(), control); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Peer("machine_test"); err != nil {
		t.Fatal("verified peer was not restored", err)
	}
	if requests != 0 {
		t.Fatal("listener startup waited for control-plane registration before restoring verified authority")
	}
	// Exercise the carrier's next startup steps too: regional discovery is not
	// persisted authority, so no relay may be admitted until fresh signed grants arrive.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	raw, err := x509.CreateCertificate(rand.Reader, cert, cert, public, private)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: private}}}
	regions, err := restarted.ConfigureRegionalRelays(tlsConfig)
	if err != nil || len(regions) != 0 {
		t.Fatalf("cached network could not start before regional discovery: %v", err)
	}
	listener, err := restarted.Listen(nil)
	if err != nil {
		t.Fatal("verified peer listener could not open", err)
	}
	defer listener.Close()
	if restarted.relay.recovery == nil {
		t.Fatal("startup omitted later authenticated relay recovery")
	}
	// A new endpoint binding must never inherit authority for the old certificate.
	changed := options
	changed.Self.EndpointGeneration++
	other, err := NewAuthority(changed)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.RestoreOrRegister(t.Context(), control); err == nil {
		t.Fatal("changed identity restored another endpoint's authority")
	}
	if _, err := other.Peer("machine_test"); err == nil {
		t.Fatal("changed identity admitted the cached peer")
	}
}

func TestExplicitRefusalCannotBeUndoneByAnOfflineRestart(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			a, cfg, signer, _ := networkTestAuthority(t)
			if err := a.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
				t.Fatal(err)
			}
			refused := networkAPIFunc{configuration: func(context.Context, string) (string, error) {
				return "", &api.APIError{Status: status, Code: "peer_network_denied"}
			}}
			if err := a.Refresh(t.Context(), refused); err == nil {
				t.Fatal("server refusal was hidden")
			}
			if _, err := a.Peer("machine_test"); err == nil {
				t.Fatal("refused live authority retained")
			}
			next, err := NewAuthority(a.options)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			offline := networkAPIFunc{register: func(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
				return api.PeerNetworkRegistrationResult{}, context.DeadlineExceeded
			}}
			if err := next.RestoreOrRegister(t.Context(), offline); err == nil {
				t.Fatal("offline restart restored explicitly refused authority")
			}
			if _, err := next.Peer("machine_test"); err == nil {
				t.Fatal("offline restart revived a revoked peer")
			}
			cfg.Generation++
			if err := next.Apply(t.Context(), networkToken(t, signer, cfg)); err != nil {
				t.Fatal("new signed authority could not recover", err)
			}
		})
	}
}
