package diagnostics

import (
	"sync/atomic"

	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
)

// The platform counters are series of the control plane's one metrics registry
// (telemetry.PlatformMetricNames). The registry is created here with a
// default instance so packages can count before the app exists; the app binds its
// own so a single instance carries typed and platform series.
var process atomic.Pointer[sharedmetrics.Metrics]

func init() { process.Store(telemetry.NewMetrics()) }

// Bind makes registry the one that platform counters write to and /metrics reads.
func Bind(registry *sharedmetrics.Metrics) { process.Store(registry) }

func count(name string, delta int64) {
	if delta > 0 {
		_ = process.Load().AddCounter(name, nil, uint64(delta))
	}
}

func raise(name string, value int64) {
	if value > 0 {
		_ = process.Load().RaiseGauge(name, nil, uint64(value))
	}
}

func DeviceRequested() { count("device_requested_total", 1) }
func DeviceCompleted(latencyMS int64) {
	count("device_completed_total", 1)
	count("device_login_latency_ms_total", latencyMS)
	raise("device_login_latency_ms_max", latencyMS)
}
func ConnectAttempted()                 { count("connect_attempts_total", 1) }
func ConnectApproved()                  { count("connect_approved_total", 1) }
func ConnectDenied()                    { count("connect_denied_total", 1) }
func RouteReady()                       { count("route_ready_total", 1) }
func CredentialsMinted()                { count("credentials_minted_total", 1) }
func RevocationPropagated()             { count("revocations_propagated_total", 1) }
func TerminalSessionCreated()           { count("terminal_sessions_created_total", 1) }
func TerminalSessionClosed()            { count("terminal_sessions_closed_total", 1) }
func TerminalSessionDeleted()           { count("terminal_sessions_deleted_total", 1) }
func TerminalOperationApplied()         { count("terminal_operations_applied_total", 1) }
func TerminalOperationRetried()         { count("terminal_operation_retries_total", 1) }
func TerminalOperationAlerted()         { count("terminal_operation_alerts_total", 1) }
func TerminalSnapshot()                 { count("terminal_snapshots_total", 1) }
func TerminalSnapshotFailed()           { count("terminal_snapshot_failures_total", 1) }
func ControlRouteObserved(count_ int64) { count("control_route_observations_total", count_) }
func ControlRouteDetached(count_ int64) { count("control_route_detaches_total", count_) }
func ControlNodeStale(count_ int64)     { count("control_node_stale_total", count_) }
func ControlUsageReceipt()              { count("control_usage_receipts_total", 1) }
func ControlCredentialIssued()          { count("control_credentials_issued_total", 1) }
func ControlRevocationFetched()         { count("control_revocation_fetches_total", 1) }

// MetricsSnapshot returns every platform metric by its flat name; a metric not
// yet counted reads 0.
func MetricsSnapshot() map[string]int64 {
	names := telemetry.PlatformMetricNames()
	result := make(map[string]int64, len(names))
	for _, name := range names {
		result[name] = 0
	}
	for _, sample := range process.Load().Snapshot() {
		if _, ok := result[sample.Name]; ok {
			result[sample.Name] = int64(sample.Value)
		}
	}
	return result
}

// MetricNames returns the sorted flat names of the platform metrics.
func MetricNames() []string { return telemetry.PlatformMetricNames() }
