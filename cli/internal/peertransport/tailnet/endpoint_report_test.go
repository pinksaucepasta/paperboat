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
	for range 2 {
		if err := a.Refresh(t.Context(), client); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen[0].Endpoints) != 0 {
		t.Fatalf("the first request reported %v before the server said it takes reports", seen[0].Endpoints)
	}
	report := seen[1].Endpoints
	if len(report) == 0 || len(report) > MaxPeerEndpoints || !validPeerEndpoints(report) {
		t.Fatalf("the second request reported %v, want the engine's normalized candidates", report)
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
	a.endpointsChanged()
	first := a.reportTimer
	a.endpointsChanged()
	if first == nil || a.reportTimer != first {
		t.Fatal("changes while a report is pending scheduled another one")
	}
	first.Stop()
}
