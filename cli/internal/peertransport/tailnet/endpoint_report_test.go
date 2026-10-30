package tailnet

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"tailscale.com/types/key"
)

// N5: the server stores the engine's own endpoints only once it says so (endpoint_reports): request bodies
// decode strictly, so reporting before that would fail every configuration call. After it, each renewal
// carries the engine's normalized candidates.
func TestEndpointsAreReportedOnlyAfterTheServerTakesThem(t *testing.T) {
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
	client := networkAPIFunc{reports: true, requests: func(r api.PeerNetworkConfigurationRequest) {
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
	if seen[0].Endpoints != nil {
		t.Fatalf("the first request reported %v before the server said it takes reports", *seen[0].Endpoints)
	}
	if seen[1].Endpoints == nil || len(*seen[1].Endpoints) == 0 || !validPeerEndpoints(*seen[1].Endpoints) {
		t.Fatalf("the second request reported %v, want the engine's normalized candidates", seen[1].Endpoints)
	}
	if seen[2].Endpoints != nil {
		t.Fatalf("an unchanged set was reported again: %v", *seen[2].Endpoints)
	}
	// The engine's candidates are gone: say so explicitly, so peers stop dialing the stale ones.
	a.mu.Lock()
	a.clientEngine = nil
	a.mu.Unlock()
	if due := a.endpointReportDue(); due == nil || len(*due) != 0 {
		t.Fatalf("a machine without candidates reports %v, want an explicit empty set", due)
	}
}

// A changed endpoint set schedules one renewal, debounced, and none while the server does not take reports.
func TestEndpointChangesScheduleOneReport(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	a.endpointsChanged()
	if a.reportTimer != nil {
		t.Fatal("a report was scheduled although the server does not take reports")
	}
	a.endpointReports.Store(true)
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

// The first report after endpoint_reports goes out even with no candidate, so the server marks this client
// as accepting delivered peer endpoints.
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

// A freshly started engine is marked as accepting peer endpoints only once it sends `endpoints`; until then its
// configuration carries none and a dial with no usable relay fails. Once the server offers endpoint_reports,
// the next request goes out at once (on-demand spacing), not after the change debounce.
func TestTheFirstEndpointReportFollowsTheOfferAtOnce(t *testing.T) {
	a, c, signer, _ := networkTestAuthority(t)
	requests := make(chan api.PeerNetworkConfigurationRequest, 8)
	client := networkAPIFunc{reports: true, requests: func(r api.PeerNetworkConfigurationRequest) { requests <- r }, configuration: func(context.Context, string) (string, error) {
		c.Generation++
		c.IssuedAt = time.Now().Unix()
		c.ExpiresAt = c.IssuedAt + 60
		c.Peers[0].Scopes[0].ExpiresAt = c.ExpiresAt
		return networkToken(t, signer, c), nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, client, nil) }()
	defer func() { cancel(); <-done }()
	first := <-requests
	if first.Endpoints != nil {
		t.Fatal("the first request reported before the offer")
	}
	select {
	case second := <-requests:
		if second.Endpoints == nil {
			t.Fatal("the request after the offer carried no endpoints")
		}
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("no report within 2.5 s of the offer")
	}
}

func TestStartupEndpointChangesDoNotWaitForSteadyStateBudget(t *testing.T) {
	a, _, _, _ := networkTestAuthority(t)
	a.endpointReports.Store(true)
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
