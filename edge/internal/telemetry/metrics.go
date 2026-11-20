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

	MetricConnectorSessions         = "paperboat_edge_connector_sessions"
	MetricConnectorConnections      = "paperboat_edge_connector_connections_total"
	MetricConnectorReconnects       = "paperboat_edge_connector_reconnects_total"
	MetricConnectorBackoff          = "paperboat_edge_connector_backoff_seconds"
	MetricConnectorHandshake        = "paperboat_edge_connector_handshake_latency_seconds"
	MetricConnectorDisconnects      = "paperboat_edge_connector_disconnects_total"
	MetricDesiredConfigGeneration   = "paperboat_edge_config_desired_generation"
	MetricAppliedConfigGeneration   = "paperboat_edge_config_applied_generation"
	MetricActiveStreams             = "paperboat_edge_active_streams"
	MetricQueueDepth                = "paperboat_edge_queue_depth"
	MetricFlowControlStalls         = "paperboat_edge_flow_control_stalls_total"
	MetricTrafficBytes              = "paperboat_edge_traffic_bytes_total"
	MetricStreamDuration            = "paperboat_edge_stream_duration_seconds"
	MetricStreamCancellations       = "paperboat_edge_stream_cancellations_total"
	MetricRouteErrors               = "paperboat_edge_route_errors_total"
	MetricRouteLatency              = "paperboat_edge_route_latency_seconds"
	MetricOriginFailures            = "paperboat_edge_origin_connect_failures_total"
	MetricProtocolUpgrades          = "paperboat_edge_protocol_upgrades_total"
	MetricHealthProbes              = "paperboat_edge_health_probes_total"
	MetricDomainVerification        = "paperboat_edge_domain_verification_seconds"
	MetricDomainVerificationFails   = "paperboat_edge_domain_verification_failures_total"
	MetricCertificateExpiryHorizon  = "paperboat_edge_certificate_expiry_horizon_seconds"
	MetricCertificateFailures       = "paperboat_edge_certificate_failures_total"
	MetricServiceUptime             = "paperboat_edge_service_uptime_seconds"
	MetricServiceRestarts           = "paperboat_edge_service_restarts_total"
	MetricWatchdogFailures          = "paperboat_edge_watchdog_failures_total"
	MetricCrashLoop                 = "paperboat_edge_crash_loop"
	MetricPreviewAllocations        = "paperboat_edge_preview_allocations_total"
	MetricPreviewLeaseCleanup       = "paperboat_edge_preview_lease_cleanup_total"
	MetricOperationIdempotencyReuse = "paperboat_edge_operation_idempotency_reuse_total"
	MetricOperationConflicts        = "paperboat_edge_operation_conflicts_total"
	// MetricForward counts visitors this edge forwarded to another managed node, by outcome (edge/internal/forward).
	MetricForward = "paperboat_edge_forward_total"
)

var localMetricDescriptors = []sharedmetrics.Descriptor{
	// Connector lifecycle. Values are intentionally role/protocol/reason
	// buckets, never connector, host, route or session identifiers.
	{Name: MetricConnectorSessions, Kind: sharedmetrics.Gauge, Labels: []sharedmetrics.LabelDescriptor{{Name: "state", AllowedValues: []string{"attached", "draining", "offline"}}}},
	{Name: MetricConnectorConnections, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "transport", AllowedValues: []string{"quic", "wss"}}, {Name: "outcome", AllowedValues: []string{"success", "failed", "canceled"}}}},
	{Name: MetricConnectorReconnects, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "reason", AllowedValues: []string{"network", "control", "lease", "stale", "shutdown", "unknown"}}}},
	{Name: MetricConnectorBackoff, Kind: sharedmetrics.Gauge},
	{Name: MetricConnectorHandshake, Kind: sharedmetrics.Histogram, Labels: []sharedmetrics.LabelDescriptor{{Name: "transport", AllowedValues: []string{"quic", "wss"}}}, Histogram: &sharedmetrics.HistogramDescriptor{Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10}}},
	{Name: MetricConnectorDisconnects, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "reason", AllowedValues: []string{"remote", "auth", "timeout", "shutdown", "stale", "protocol", "error", "unknown"}}}},

	// Configuration and stream resource state.
	{Name: MetricDesiredConfigGeneration, Kind: sharedmetrics.Gauge},
	{Name: MetricAppliedConfigGeneration, Kind: sharedmetrics.Gauge},
	{Name: MetricActiveStreams, Kind: sharedmetrics.Gauge},
	{Name: MetricQueueDepth, Kind: sharedmetrics.Gauge, Labels: []sharedmetrics.LabelDescriptor{{Name: "queue", AllowedValues: []string{"control", "route", "stream", "usage", "telemetry"}}}},
	{Name: MetricFlowControlStalls, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "direction", AllowedValues: []string{"ingress", "egress"}}}},
	{Name: MetricTrafficBytes, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "direction", AllowedValues: []string{"ingress", "egress"}}}},
	{Name: MetricStreamDuration, Kind: sharedmetrics.Histogram, Labels: []sharedmetrics.LabelDescriptor{{Name: "route_kind", AllowedValues: []string{"preview_public_https_wss", "tunnel_https_wss", "tunnel_tcp", "tunnel_private_tcp"}}}, Histogram: &sharedmetrics.HistogramDescriptor{Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 900}}},
	{Name: MetricStreamCancellations, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "reason", AllowedValues: []string{"client", "origin", "edge", "timeout", "shutdown", "unknown"}}}},

	// Route, origin and probe outcomes use only fixed protocol/reason buckets.
	{Name: MetricRouteErrors, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "route_kind", AllowedValues: []string{"preview_public_https_wss", "tunnel_https_wss", "tunnel_tcp", "tunnel_private_tcp"}}, {Name: "reason", AllowedValues: []string{"not_found", "rejected", "origin", "timeout", "protocol", "internal"}}}},
	{Name: MetricRouteLatency, Kind: sharedmetrics.Histogram, Labels: []sharedmetrics.LabelDescriptor{{Name: "route_kind", AllowedValues: []string{"preview_public_https_wss", "tunnel_https_wss", "tunnel_tcp", "tunnel_private_tcp"}}}, Histogram: &sharedmetrics.HistogramDescriptor{Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10}}},
	{Name: MetricOriginFailures, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "protocol", AllowedValues: []string{"http1", "http2", "h2c", "tcp", "websocket"}}, {Name: "reason", AllowedValues: []string{"refused", "timeout", "tls", "reset", "unavailable", "unknown"}}}},
	{Name: MetricProtocolUpgrades, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "from", AllowedValues: []string{"http1", "http2", "h2c", "tcp"}}, {Name: "to", AllowedValues: []string{"http2", "h2c", "websocket", "tcp"}}}},
	{Name: MetricHealthProbes, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "dimension", AllowedValues: []string{"service", "edge", "config", "route", "origin", "dns", "certificate", "access", "update"}}, {Name: "outcome", AllowedValues: []string{"success", "failed", "timeout", "canceled"}}}},
	{Name: MetricDomainVerification, Kind: sharedmetrics.Histogram, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: []string{"success", "failed", "timeout"}}}, Histogram: &sharedmetrics.HistogramDescriptor{Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 900}}},
	{Name: MetricDomainVerificationFails, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "reason", AllowedValues: []string{"dns", "conflict", "timeout", "provider", "unknown"}}}},

	// Certificate, process, preview and operation lifecycle.
	{Name: MetricCertificateExpiryHorizon, Kind: sharedmetrics.Gauge},
	{Name: MetricCertificateFailures, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "operation", AllowedValues: []string{"issue", "renew", "replace", "revoke"}}, {Name: "reason", AllowedValues: []string{"dns", "acme", "expired", "revoked", "distribution", "unknown"}}}},
	{Name: MetricServiceUptime, Kind: sharedmetrics.Gauge},
	{Name: MetricServiceRestarts, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "reason", AllowedValues: []string{"crash", "upgrade", "shutdown", "unknown"}}}},
	{Name: MetricWatchdogFailures, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "reason", AllowedValues: []string{"timeout", "crash", "unhealthy", "unknown"}}}},
	{Name: MetricCrashLoop, Kind: sharedmetrics.Gauge},
	{Name: MetricPreviewAllocations, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: sharedmetrics.PreviewAllocationOutcomes()}}},
	{Name: MetricPreviewLeaseCleanup, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "reason", AllowedValues: []string{"stop", "expiry", "owner_lost", "edge_lost", "shutdown", "unknown"}}}},
	{Name: MetricOperationIdempotencyReuse, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "operation", AllowedValues: []string{"create", "update", "delete", "reconcile", "certificate", "access"}}}},
	{Name: MetricForward, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "outcome", AllowedValues: []string{"forwarded", "holder_miss", "unreachable", "loop_refused", "refused"}}}},
	{Name: MetricOperationConflicts, Kind: sharedmetrics.Counter, Labels: []sharedmetrics.LabelDescriptor{{Name: "operation", AllowedValues: []string{"create", "update", "delete", "reconcile", "certificate", "access"}}}},
}

var fixedMetricDescriptors = append(sharedmetrics.ContractDescriptors(), localMetricDescriptors...)

// MetricDescriptors returns an independent copy of every descriptor this binary exports.
func MetricDescriptors() []sharedmetrics.Descriptor {
	return sharedmetrics.Clone(fixedMetricDescriptors)
}

// NewMetrics returns the metric store for this binary's descriptors.
func NewMetrics() *sharedmetrics.Metrics { return sharedmetrics.New(fixedMetricDescriptors, 0) }
