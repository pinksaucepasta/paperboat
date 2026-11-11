package tailnet

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"tailscale.com/types/key"
)

// N5: the engine reports its normalized candidates from its first configuration request (so peers reach it directly
// at once, F20-02/04), then only when they change.
func TestEndpointsAreReportedFromTheFirstRequest(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	engine := &mesh.Server{Key: key.NewNode(), LocalAddr: netip.MustParseAddr("fd7a:115c:a1e0::30"), AllowedPeers: map[key.NodePublic]netip.Addr{}, Logf: func(string, ...any) {}}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for len(engine.LocalEndpoints()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(engine.LocalEndpoints()) == 0 {
		t.Fatal("the engine found no local endpoint")
	}
	a.mu.Lock()
	a.clientEngine = engine
	a.mu.Unlock()
	t.Cleanup(func() { a.mu.Lock(); a.clientEngine = nil; a.mu.Unlock() })
	var mu sync.Mutex
	var seen []api.PeerNetworkConfigurationRequest
	client := networkAPIFunc{requests: func(r api.PeerNetworkConfigurationRequest) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	}, configuration: func(context.Context, string) (string, error) {
		c.Generation++
		c.IssuedAt = time.Now().Unix()
		c.ExpiresAt = c.IssuedAt + 60
		c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt
		return networkToken(t, signer, c), nil
	}}
	for range 3 {
		if err := a.Refresh(t.Context(), client); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[0].Endpoints == nil || len(*seen[0].Endpoints) == 0 || !peernetwork.ValidPeerEndpoints(*seen[0].Endpoints) {
		t.Fatalf("the first request reported %v, want the engine's normalized candidates", seen[0].Endpoints)
	}
	if seen[1].Endpoints != nil || seen[2].Endpoints != nil {
		t.Fatalf("an unchanged set was reported again: %v %v", seen[1].Endpoints, seen[2].Endpoints)
	}
	// The engine's candidates are gone: say so explicitly, so peers stop dialing the stale ones.
	a.mu.Lock()
	a.clientEngine = nil
	a.mu.Unlock()
	if due := a.endpointReportDue(); due == nil || len(*due) != 0 {
		t.Fatalf("a machine without candidates reports %v, want an explicit empty set", due)
	}
}

// Before the first non-empty report, a changed endpoint set renews at once (urgent), with no debounce timer.
func TestEndpointChangesScheduleOneReport(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	a.endpointsChanged()
	if a.reportTimer != nil {
		t.Fatal("a startup endpoint change waited for the debounce")
	}
	a.endpointsReported([]string{"192.0.2.1:1234"})
	a.endpointsChanged()
	first := a.reportTimer
	a.endpointsChanged()
	if first == nil || a.reportTimer != first {
		t.Fatal("changes while a report is pending scheduled another one")
	}
	first.Stop()
}

// Membership grants are asked for only after the server says it can issue them, so a server that cannot
// never receives the field (request bodies decode strictly) and an older client never gets grants it cannot
// verify.
func TestMembershipGrantsAreAskedForOnlyAfterTheServerOffersThem(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	var seen []api.PeerNetworkConfigurationRequest
	offer := false
	client := membershipOffer{offer: &offer, seen: &seen, configuration: func() (string, error) {
		c.Generation++
		c.IssuedAt = time.Now().Unix()
		c.ExpiresAt = c.IssuedAt + 60
		c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt
		return networkToken(t, signer, c), nil
	}}
	for _, o := range []bool{false, true, true} {
		offer = o
		if err := a.Refresh(t.Context(), client); err != nil {
			t.Fatal(err)
		}
	}
	if seen[0].MembershipGrants || seen[1].MembershipGrants || !seen[2].MembershipGrants {
		t.Fatalf("asked %v %v %v, want only after the offer", seen[0].MembershipGrants, seen[1].MembershipGrants, seen[2].MembershipGrants)
	}
}

type membershipOffer struct {
	offer         *bool
	seen          *[]api.PeerNetworkConfigurationRequest
	configuration func() (string, error)
}

func (m membershipOffer) RegisterPeerNetwork(context.Context, api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return api.PeerNetworkRegistrationResult{}, nil
}

func (m membershipOffer) PeerNetworkConfiguration(_ context.Context, r api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	*m.seen = append(*m.seen, r)
	token, err := m.configuration()
	return api.PeerNetworkConfigurationResult{Configuration: token, MembershipGrants: *m.offer}, err
}

// The first report goes out even with no candidate: a machine whose candidates are gone says so.
func TestTheFirstEndpointReportIsSentEvenWhenEmpty(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	if due := a.endpointReportDue(); due == nil || len(*due) != 0 {
		t.Fatalf("first report %v, want an explicit empty set", due)
	}
	a.endpointsReported([]string{})
	if due := a.endpointReportDue(); due != nil {
		t.Fatalf("an unchanged empty set was reported again: %v", *due)
	}
}

func TestStartupEndpointChangesDoNotWaitForSteadyStateBudget(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	a.endpointsReported(nil)
	a.endpointsChanged()
	select {
	case <-a.refreshUrgent:
	default:
		t.Fatal("startup endpoint change waited for steady-state debounce and renewal budget")
	}
	a.endpointsReported([]string{"192.0.2.1:1234"})
	a.endpointsChanged()
	if a.reportTimer == nil {
		t.Fatal("steady-state endpoint changes lost bounded debounce")
	}
	a.reportTimer.Stop()
}

func TestAuthorityDropClearsThePreviousEngineEndpointBudget(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	a.endpointsReported([]string{"192.0.2.1:1234"})
	a.mu.Lock()
	a.dropLocked()
	a.mu.Unlock()
	a.endpointsChanged()
	defer func() {
		if a.reportTimer != nil {
			a.reportTimer.Stop()
		}
	}()
	select {
	case <-a.refreshUrgent:
	default:
		t.Fatal("replacement engine inherited the previous engine's endpoint budget")
	}
}
