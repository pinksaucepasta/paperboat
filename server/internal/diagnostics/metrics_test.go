package diagnostics

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
)

func TestMetricsSnapshotTracksBoundedPlatformCounters(t *testing.T) {
	before := MetricsSnapshot()
	DeviceRequested()
	DeviceCompleted(25)
	ConnectAttempted()
	ConnectApproved()
	ConnectDenied()
	RouteReady()
	CredentialsMinted()
	RevocationPropagated()
	TerminalSessionCreated()
	TerminalSessionClosed()
	TerminalSessionDeleted()
	TerminalOperationApplied()
	TerminalOperationRetried()
	TerminalOperationAlerted()
	TerminalSnapshot()
	TerminalSnapshotFailed()
	ControlRouteObserved(2)
	ControlRouteDetached(1)
	ControlNodeStale(1)
	ControlUsageReceipt()
	ControlCredentialIssued()
	ControlRevocationFetched()
	after := MetricsSnapshot()
	for _, key := range []string{"device_requested_total", "device_completed_total", "connect_attempts_total", "connect_approved_total", "connect_denied_total", "route_ready_total", "credentials_minted_total", "revocations_propagated_total", "terminal_sessions_created_total", "terminal_sessions_closed_total", "terminal_sessions_deleted_total", "terminal_operations_applied_total", "terminal_operation_retries_total", "terminal_operation_alerts_total", "terminal_snapshots_total", "terminal_snapshot_failures_total"} {
		if after[key] != before[key]+1 {
			t.Fatalf("%s = %d, want %d", key, after[key], before[key]+1)
		}
	}
	if after["device_login_latency_ms_total"] != before["device_login_latency_ms_total"]+25 {
		t.Fatalf("latency total = %d", after["device_login_latency_ms_total"])
	}
	if after["device_login_latency_ms_max"] < 25 {
		t.Fatalf("latency max = %d", after["device_login_latency_ms_max"])
	}
	if after["control_route_observations_total"] != before["control_route_observations_total"]+2 {
		t.Fatal("route observations not tracked")
	}
	for _, key := range []string{"control_route_detaches_total", "control_node_stale_total", "control_usage_receipts_total", "control_credentials_issued_total", "control_revocation_fetches_total"} {
		if after[key] != before[key]+1 {
			t.Fatalf("%s not tracked", key)
		}
	}
}

// Platform counters are series of the bound registry: the same instance serves the
// typed catalog, every flat name is always present, and an unbound-then-bound
// registry starts from zero.
func TestPlatformCountersLiveInTheBoundRegistry(t *testing.T) {
	registry := telemetry.NewMetrics()
	Bind(registry)
	DeviceCompleted(7)
	DeviceCompleted(3)
	snapshot := MetricsSnapshot()
	if len(snapshot) != len(telemetry.PlatformMetricNames()) {
		t.Fatalf("snapshot has %d names, want %d", len(snapshot), len(telemetry.PlatformMetricNames()))
	}
	if snapshot["device_completed_total"] != 2 || snapshot["device_login_latency_ms_total"] != 10 || snapshot["device_login_latency_ms_max"] != 7 {
		t.Fatalf("device metrics = %v", snapshot)
	}
	if err := registry.IncCounter(telemetry.MetricAccessDecisions, sharedmetrics.Labels{"decision": "allowed"}); err != nil {
		t.Fatal(err)
	}
	if got := len(telemetry.TypedSamples(registry)); got != 1 {
		t.Fatalf("typed samples = %d, want only the access decision", got)
	}
	Bind(telemetry.NewMetrics())
	if MetricsSnapshot()["device_completed_total"] != 0 {
		t.Fatal("a freshly bound registry did not start at zero")
	}
}
