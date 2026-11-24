package tailnet

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// unchangedServer answers "unchanged" to the generation it last signed, like the server.
type unchangedServer struct {
	generation uint64
	token      string
	during     func() // runs while an "unchanged" request is in flight
}

func (unchangedServer) RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return api.PeerNetworkRegistrationResult{}, nil
}

func (s unchangedServer) PeerNetworkConfiguration(_ context.Context, r api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	if r.BaseGeneration == s.generation {
		if s.during != nil {
			s.during()
		}
		return api.PeerNetworkConfigurationResult{Unchanged: true}, nil
	}
	return api.PeerNetworkConfigurationResult{Configuration: s.token}, nil
}

// A restarted process holds its last configuration only on disk. Claiming it as the base let the server answer
// "unchanged", and the first command after every daemon start then ran on no configuration at all.
func TestRestartNeverClaimsAnUnloadedBase(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	token := networkToken(t, signer, cfg)
	answer := unchangedServer{generation: cfg.Generation, token: token}
	if err := a.Refresh(t.Context(), answer); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(t.Context(), answer); err != nil { // loaded: "unchanged" keeps it
		t.Fatal(err)
	}
	restarted, err := NewAuthority(AuthorityOptions{Store: a.options.Store, Issuer: a.options.Issuer, Self: cfg.Self, Keys: a.options.Keys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	_ = a.Close()
	if err := restarted.RegisterOrRestore(t.Context(), answer, false); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Peer("machine_test"); err != nil {
		t.Fatal("the restarted authority has no configuration:", err)
	}
}

// A configuration dropped (revoked) while the request ran leaves nothing for "unchanged" to keep.
func TestUnchangedAnswerAfterADropIsAskedInFull(t *testing.T) {
	a, cfg, signer, _ := networkTestAuthority(t)
	answer := unchangedServer{generation: cfg.Generation, token: networkToken(t, signer, cfg)}
	if err := a.Refresh(t.Context(), answer); err != nil {
		t.Fatal(err)
	}
	answer.during = func() {
		a.mu.Lock()
		a.dropLocked()
		a.mu.Unlock()
	}
	if err := a.Refresh(t.Context(), answer); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Peer("machine_test"); err != nil {
		t.Fatal("an unchanged answer left the authority without a configuration:", err)
	}
}
