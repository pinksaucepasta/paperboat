package telemetry

import (
	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
)

const (
	MetricAccessDecisions   = sharedmetrics.AccessDecisions
	MetricCertificateOps    = sharedmetrics.CertificateOps
	MetricHealthDimension   = sharedmetrics.HealthDimension
	MetricHealthTransitions = sharedmetrics.HealthTransitions
	MetricOriginConnections = sharedmetrics.OriginConnections
	MetricEdgeRequests      = sharedmetrics.EdgeRequests
	MetricRouteRequests     = sharedmetrics.RouteRequests
	MetricUpdateOperations  = sharedmetrics.UpdateOperations
	MetricOperationLatency  = sharedmetrics.OperationLatency

	// Server-owned diagnostics names. Labels below are finite enums only;
	// account, tunnel, route, hostname, URL, request, and credential values are
	// deliberately not representable in this registry.
	MetricDNSVerificationDuration  = "paperboat_server_dns_verification_duration_seconds"
	MetricDNSVerificationFailures  = "paperboat_server_dns_verification_failures_total"
	MetricCertificateExpiryHorizon = "paperboat_server_certificate_expiry_horizon_seconds"
	MetricPreviewAllocation        = "paperboat_server_preview_allocations_total"
	MetricPreviewLeaseCleanup      = "paperboat_server_preview_lease_cleanup_total"
	MetricOperationIdempotency     = "paperboat_server_operation_idempotency_total"
	MetricServiceUptime            = "paperboat_server_service_uptime_seconds"
	MetricWorkerMode               = "paperboat_server_worker_mode"
	MetricServiceRestarts          = "paperboat_server_service_restarts_total"
	MetricServiceWatchdog          = "paperboat_server_service_watchdog_total"
	MetricServiceCrashLoop         = "paperboat_server_service_crash_loop"
	MetricConfigGenerations        = "paperboat_server_config_generations"
	MetricRouteErrors              = "paperboat_server_route_errors_total"
	MetricRouteLatency             = "paperboat_server_route_latency_seconds"
	MetricHTTPRequests             = "paperboat_server_http_requests_total"
	MetricHTTPDuration             = "paperboat_server_http_request_duration_seconds"
	MetricRelaySubjectObservations = "paperboat_server_relay_subject_observations_total"
)

var localMetricDescriptors = []sharedmetrics.Descriptor{
	{Name: MetricDNSVerificationDuration, Kind: sharedmetrics.Histogram, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: []string{"success", "failed", "waiting"}}}, Histogram: &sharedmetrics.HistogramDescriptor{Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600}}},
	{Name: MetricDNSVerificationFailures, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "failure_class", AllowedValues: []string{"nxdomain", "timeout", "conflict", "provider", "unauthorized", "invalid", "unknown"}}}},
	{Name: MetricCertificateExpiryHorizon, Kind: sharedmetrics.Gauge, Labels: []sharedmetrics.LabelDescriptor{{Name: "horizon", AllowedValues: []string{"expired", "under_1h", "under_24h", "under_7d", "under_14d", "over_14d"}}}},
	{Name: MetricRelaySubjectObservations, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "shape", AllowedValues: []string{"full", "digest_only", "required"}}}},
	{Name: MetricPreviewAllocation, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: sharedmetrics.PreviewAllocationOutcomes()}}},
	{Name: MetricPreviewLeaseCleanup, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: []string{"success", "failed", "already_clean"}}}},
	{Name: MetricOperationIdempotency, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: []string{"new", "reused", "conflict"}}}},
	{Name: MetricWorkerMode, Kind: sharedmetrics.Gauge, Labels: []sharedmetrics.LabelDescriptor{{Name: "mode", AllowedValues: []string{"leader", "standby", "drained"}}}},
	{Name: MetricServiceUptime, Kind: sharedmetrics.Gauge, Labels: []sharedmetrics.LabelDescriptor{{Name: "state", AllowedValues: []string{"running", "degraded", "stopped"}}}},
	{Name: MetricServiceRestarts, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: []string{"success", "failed"}}}},
	{Name: MetricServiceWatchdog, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: []string{"healthy", "failed", "restarted"}}}},
	{Name: MetricServiceCrashLoop, Kind: sharedmetrics.Gauge, Labels: []sharedmetrics.LabelDescriptor{{Name: "state", AllowedValues: []string{"clear", "detected"}}}},
	{Name: MetricConfigGenerations, Kind: sharedmetrics.Gauge, Labels: []sharedmetrics.LabelDescriptor{{Name: "state", AllowedValues: []string{"desired", "applied"}}}},
	{Name: MetricRouteErrors, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "class", AllowedValues: []string{"not_found", "conflict", "paused", "unauthorized", "origin", "edge", "internal"}}}},
	{Name: MetricRouteLatency, Kind: sharedmetrics.Histogram, Labels: []sharedmetrics.LabelDescriptor{{Name: "protocol", AllowedValues: []string{"http", "https", "h2c", "tcp", "websocket"}}}, Histogram: &sharedmetrics.HistogramDescriptor{Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10}}},
	{Name: MetricHTTPRequests, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{
		{Name: "method", AllowedValues: []string{"get", "post", "put", "patch", "delete", "other"}},
		{Name: "route_family", AllowedValues: []string{"health", "public_api", "edge_control", "release", "internal", "other"}},
		{Name: "status_class", AllowedValues: []string{"1xx", "2xx", "3xx", "4xx", "5xx"}},
	}},
	{Name: MetricHTTPDuration, Kind: sharedmetrics.Histogram, Labels: []sharedmetrics.LabelDescriptor{
		{Name: "route_family", AllowedValues: []string{"health", "public_api", "edge_control", "release", "internal", "other"}},
		{Name: "status_class", AllowedValues: []string{"1xx", "2xx", "3xx", "4xx", "5xx"}},
	}, Histogram: &sharedmetrics.HistogramDescriptor{Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10}}},
}

var fixedMetricDescriptors = append(sharedmetrics.ContractDescriptors(), localMetricDescriptors...)

// MetricDescriptors returns an independent copy of every descriptor the control plane exports.
func MetricDescriptors() []sharedmetrics.Descriptor {
	return sharedmetrics.Clone(fixedMetricDescriptors)
}

// NewMetrics returns the control plane's metric store.
func NewMetrics() *sharedmetrics.Metrics { return sharedmetrics.New(fixedMetricDescriptors, 0) }

// NewMetricsWithLimit is NewMetrics with at most maxSeries live series.
func NewMetricsWithLimit(maxSeries int) *sharedmetrics.Metrics {
	return sharedmetrics.New(fixedMetricDescriptors, maxSeries)
}
