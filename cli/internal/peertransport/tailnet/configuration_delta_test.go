package tailnet

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"tailscale.com/types/key"
)

// deltaServer answers like the server's sentConfigurations: a delta when the request's base is the configuration it
// last sent, else the full token. tamper, when set, changes a delta after it was built.
type deltaServer struct {
	t      *testing.T
	signer ed25519.PrivateKey
	cfg    NetworkConfiguration
	sent   *NetworkConfiguration
	bases  []uint64
	deltas int
	tamper func(*peernetwork.ConfigurationDelta)
}

func (s *deltaServer) RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return api.PeerNetworkRegistrationResult{}, errors.New("not used")
}

func (s *deltaServer) PeerNetworkConfiguration(_ context.Context, r api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	s.bases = append(s.bases, r.BaseGeneration)
	s.cfg.Generation++
	token := networkToken(s.t, s.signer, s.cfg)
	var result api.PeerNetworkConfigurationResult
	if s.sent != nil && r.BaseGeneration == s.sent.Generation {
		d := peernetwork.NewConfigurationDelta(token, s.cfg, r.BaseGeneration, peernetwork.PeerDigests(s.sent.Peers))
		if s.tamper != nil {
			s.tamper(&d)
		}
		result.ConfigurationDelta, s.deltas = &d, s.deltas+1
	} else {
		result.Configuration = token
	}
	sent := s.cfg
	s.sent = &sent
	return result, nil
}

// A peer joining or leaving reaches the endpoint as a delta that rebuilds the server's exact signed configuration;
// a revocation (a removed peer) applies at once, and a delta that does not verify or apply is asked again in full.
func TestRefreshAppliesConfigurationDeltas(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	cfg.Generation = 0
	server := &deltaServer{t: t, signer: signer, cfg: cfg}
	ctx := t.Context()
	refresh := func() {
		t.Helper()
		if err := a.Refresh(ctx, server); err != nil {
			t.Fatal(err)
		}
	}
	held := func() string {
		token, err := a.options.Store.LoadPeerNetworkConfiguration(a.options.Issuer, a.options.Self.AccountID, a.options.Self.EndpointID, MaxConfigurationBytes)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	refresh() // nothing held: base 0, full configuration
	if server.bases[0] != 0 || server.deltas != 0 {
		t.Fatalf("first refresh sent base %d and got %d deltas", server.bases[0], server.deltas)
	}

	joined := cfg.Peers[0]
	joined.Identity = networkTestBinding("machine", "machine_two", "fd7a:115c:a1e0::4", key.NewNode())
	joined.Scopes = []NetworkScope{{ResourceKind: "machine_access", ResourceID: "grant_two", ResourceGeneration: 1, Capability: "terminal", Direction: "dial", Port: 443, ExpiresAt: cfg.ExpiresAt}}
	server.cfg.Peers = append(server.cfg.Peers, joined)
	refresh()
	if server.bases[1] != 1 || server.deltas != 1 {
		t.Fatalf("second refresh sent base %d (want 1), %d deltas", server.bases[1], server.deltas)
	}
	if _, err := a.Peer("machine_two"); err != nil {
		t.Fatal("the joined peer from a delta is not admitted:", err)
	}
	if held() != networkToken(t, signer, server.cfg) {
		t.Fatal("the configuration rebuilt from a delta is not the server's full token")
	}

	server.cfg.Peers = server.cfg.Peers[1:] // machine_test revoked
	refresh()
	if server.deltas != 2 {
		t.Fatalf("revocation was not sent as a delta (%d deltas)", server.deltas)
	}
	if _, err := a.Peer("machine_test"); !errors.Is(err, ErrAdmission) {
		t.Fatal("a revocation delivered as a delta was not applied at once:", err)
	}

	// A delta that does not verify (a changed peer under the old signature) is asked again in full.
	server.tamper = func(d *peernetwork.ConfigurationDelta) {
		d.Configuration.Peers = append(d.Configuration.Peers, cfg.Peers[0])
	}
	refresh()
	if n := len(server.bases); server.bases[n-2] != 3 || server.bases[n-1] != 0 {
		t.Fatalf("a delta that does not verify was not asked again in full: bases %v", server.bases)
	}
	if _, err := a.Peer("machine_test"); !errors.Is(err, ErrAdmission) {
		t.Fatal("a tampered delta re-admitted a revoked peer:", err)
	}
	// A delta for another base than the held one (a gap) is asked again in full too.
	server.tamper = func(d *peernetwork.ConfigurationDelta) { d.BaseGeneration-- }
	refresh()
	if n := len(server.bases); server.bases[n-1] != 0 || held() != networkToken(t, signer, server.cfg) {
		t.Fatalf("a delta for another base was not replaced by the full configuration: bases %v", server.bases)
	}
}
