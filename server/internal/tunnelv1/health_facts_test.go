package tunnelv1

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// Each failure a tunnel can be in must say which one it is, whether it fixes itself, and what to do.
func TestStatusNamesEachFailureModeAndWhatToDo(t *testing.T) {
	healthyConnector := HealthFacts{ActiveConnectors: 1, ConnectorSessions: 1, EdgeReady: true}
	with := func(change func(*HealthFacts)) HealthFacts {
		facts := healthyConnector
		change(&facts)
		return facts
	}
	wantDimension := map[string]string{
		"no connector at all": "connector_missing", "host offline": "connector_offline", "host went offline after the tunnel was ready": "connector_offline",
		"edge unreachable": "edge_unavailable", "dns pending": "dns_pending", "certificate pending": "certificate_pending",
		"origin refused": "origin_refused", "origin timeout": "origin_timeout", "origin tls": "origin_tls",
		"route rejected": "route_rejected", "bandwidth exhausted": "bandwidth_exhausted",
	}
	for name, tc := range map[string]struct {
		summary      string
		facts        HealthFacts
		bandwidth    bool
		wantCode     string
		wantDimCode  func(HealthView) string
		wantAction   string
		wantRetrying bool
	}{
		"no connector at all": {summary: "pending", facts: HealthFacts{EdgeReady: true}, wantCode: "connector_missing",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Service.Code }, wantAction: "pb tunnel connector add"},
		"host offline": {summary: "pending", facts: HealthFacts{ActiveConnectors: 1, EdgeReady: true}, wantCode: "connector_offline",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Service.Code }, wantAction: "pb doctor", wantRetrying: true},
		"host went offline after the tunnel was ready": {summary: "ready", facts: HealthFacts{ActiveConnectors: 1, EdgeReady: true}, wantCode: "connector_offline",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Service.Code }, wantAction: "pb doctor", wantRetrying: true},
		"edge unreachable": {summary: "pending", facts: with(func(f *HealthFacts) { f.EdgeReady = false }), wantCode: "edge_unavailable",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Edge.Code }, wantAction: "pb tunnel doctor", wantRetrying: true},
		"dns pending": {summary: "pending", facts: with(func(f *HealthFacts) { f.PendingDomains = []string{"api.example.test"} }), wantCode: "dns_pending",
			wantDimCode: func(v HealthView) string { return v.Dimensions.DNS.Code }, wantAction: "pb tunnel domain instructions", wantRetrying: true},
		"certificate pending": {summary: "pending", facts: with(func(f *HealthFacts) { f.PendingCertificates = []string{"api.example.test"} }), wantCode: "certificate_pending",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Certificate.Code }, wantAction: "pb tunnel domain list", wantRetrying: true},
		"origin refused": {summary: "origin_unreachable", facts: with(func(f *HealthFacts) { f.OriginReason = "refused" }), wantCode: "origin_unreachable",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Origin.Code }, wantAction: "Start the app", wantRetrying: true},
		"origin timeout": {summary: "origin_unreachable", facts: with(func(f *HealthFacts) { f.OriginReason = "timeout" }), wantCode: "origin_unreachable",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Origin.Code }, wantAction: "not responding", wantRetrying: true},
		"origin tls": {summary: "origin_unreachable", facts: with(func(f *HealthFacts) { f.OriginReason = "tls" }), wantCode: "origin_unreachable",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Origin.Code }, wantAction: "certificate", wantRetrying: true},
		"route rejected": {summary: "route_unavailable", facts: with(func(f *HealthFacts) { f.FailedRoutes = 1 }), wantCode: "route_unavailable",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Route.Code }, wantAction: "pb tunnel route update", wantRetrying: true},
		"bandwidth exhausted": {summary: "ready", facts: healthyConnector, bandwidth: true, wantCode: "bandwidth_exhausted",
			wantDimCode: func(v HealthView) string { return v.Dimensions.Access.Code }, wantAction: "Billing"},
	} {
		t.Run(name, func(t *testing.T) {
			facts := tc.facts
			facts.BandwidthExhausted = tc.bandwidth
			repository := &fakeTunnelRepository{facts: facts, tunnel: dbsqlc.Tunnel{ID: "tun_1", AccountID: "acct_1", DesiredState: DesiredActive, AccessMode: AccessPrivate, SummaryCode: tc.summary, Generation: 1}}
			status, err := testService(t, repository, sequentialID()).Status(context.Background(), testRequest(false), "tun_1")
			if err != nil {
				t.Fatal(err)
			}
			if status.OverallCode != tc.wantCode || status.Retrying != tc.wantRetrying {
				t.Fatalf("status = %q retrying=%v, want %q retrying=%v (%+v)", status.OverallCode, status.Retrying, tc.wantCode, tc.wantRetrying, status)
			}
			if code := tc.wantDimCode(status); code != wantDimension[name] {
				t.Fatalf("the failing dimension carries %q, want %q", code, wantDimension[name])
			}
			if !strings.Contains(status.RepairAction, tc.wantAction) || status.Summary == "" || strings.Contains(status.Summary, "waiting for a connector and origin readiness") {
				t.Fatalf("status gives no specific cause or action: summary=%q action=%q", status.Summary, status.RepairAction)
			}
		})
	}
}

func TestStatusKeepsTheDesiredStateExplanationsAheadOfLiveFacts(t *testing.T) {
	for state, want := range map[string]string{DesiredPaused: "tunnel_paused", DesiredDeleted: "tunnel_deleted"} {
		repository := &fakeTunnelRepository{facts: HealthFacts{BandwidthExhausted: false}, tunnel: dbsqlc.Tunnel{ID: "tun_1", AccountID: "acct_1", DesiredState: state, AccessMode: AccessPrivate, SummaryCode: "pending", Generation: 1}}
		status, err := testService(t, repository, sequentialID()).Status(context.Background(), testRequest(false), "tun_1")
		if err != nil || status.OverallCode != want {
			t.Fatalf("%s: status=%+v err=%v", state, status, err)
		}
	}
}

func TestStatusReportsAHealthyTunnelAsReadyAndReadFailuresAsErrors(t *testing.T) {
	repository := &fakeTunnelRepository{facts: HealthFacts{ActiveConnectors: 1, ConnectorSessions: 1, EdgeReady: true}, tunnel: dbsqlc.Tunnel{ID: "tun_1", AccountID: "acct_1", DesiredState: DesiredActive, AccessMode: AccessPrivate, SummaryCode: "ready", Generation: 1}}
	status, err := testService(t, repository, sequentialID()).Status(context.Background(), testRequest(false), "tun_1")
	if err != nil || status.OverallCode != "ready" || status.RepairAction != "No action required." {
		t.Fatalf("healthy status = %+v err=%v", status, err)
	}
	repository.factsErr = errors.New("facts unavailable")
	if _, err := testService(t, repository, sequentialID()).Status(context.Background(), testRequest(false), "tun_1"); err == nil {
		t.Fatal("a failed live-facts read was reported as a healthy status")
	}
}
