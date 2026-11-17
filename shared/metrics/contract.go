package metrics

import "github.com/pinksaucepasta/paperboat/shared/health"

// The nine metrics the edge and the control plane both export under the
// paperboat_edge_ prefix. They are one dashboard contract, so they are defined
// here once: a label or bucket change is made in one place for both binaries.
const (
	AccessDecisions   = "paperboat_edge_access_decisions_total"
	CertificateOps    = "paperboat_edge_certificate_operations_total"
	HealthDimension   = "paperboat_edge_health_dimension"
	HealthTransitions = "paperboat_edge_health_transitions_total"
	OriginConnections = "paperboat_edge_origin_connections_total"
	EdgeRequests      = "paperboat_edge_requests_total"
	RouteRequests     = "paperboat_edge_route_requests_total"
	UpdateOperations  = "paperboat_edge_update_operations_total"
	OperationLatency  = "paperboat_edge_operation_latency_seconds"
)

var (
	routeKinds       = []string{"preview_public_https_wss", "tunnel_https_wss", "tunnel_tcp", "tunnel_private_tcp"}
	operationOutcome = []string{"success", "failed", "canceled"}
	requestOutcome   = []string{"success", "rejected", "failed", "canceled"}
	healthStatuses   = []string{"unknown", "ready", "degraded", "down", "not_applicable"}
)

// RouteKinds returns the route_kind label values shared by every route metric.
func RouteKinds() []string { return append([]string(nil), routeKinds...) }

// HealthStatusValues returns the status/from/to label values of the health metrics.
func HealthStatusValues() []string { return append([]string(nil), healthStatuses...) }

// DimensionValues returns the dimension label values, in canonical health order.
func DimensionValues() []string {
	dimensions := health.Dimensions()
	values := make([]string, len(dimensions))
	for index, dimension := range dimensions {
		values[index] = string(dimension)
	}
	return values
}

// ContractDescriptors returns the nine shared descriptors.
func ContractDescriptors() []Descriptor {
	return []Descriptor{
		{Name: AccessDecisions, Kind: Counter, Labels: []LabelDescriptor{{Name: "decision", AllowedValues: []string{"allowed", "denied", "unavailable"}}}},
		{Name: CertificateOps, Kind: Counter, Labels: []LabelDescriptor{{Name: "operation", AllowedValues: []string{"issue", "renew", "replace", "revoke"}}, {Name: "outcome", AllowedValues: cloneStrings(operationOutcome)}}},
		{Name: HealthDimension, Kind: Gauge, Labels: []LabelDescriptor{{Name: "dimension", AllowedValues: DimensionValues()}, {Name: "status", AllowedValues: HealthStatusValues()}}},
		{Name: HealthTransitions, Kind: Counter, Labels: []LabelDescriptor{{Name: "dimension", AllowedValues: DimensionValues()}, {Name: "from", AllowedValues: HealthStatusValues()}, {Name: "to", AllowedValues: HealthStatusValues()}}},
		{Name: OriginConnections, Kind: Counter, Labels: []LabelDescriptor{{Name: "protocol", AllowedValues: []string{"http1", "http2", "h2c", "tcp", "websocket"}}, {Name: "outcome", AllowedValues: []string{"success", "failed", "canceled", "timeout"}}}},
		{Name: EdgeRequests, Kind: Counter, Labels: []LabelDescriptor{{Name: "protocol", AllowedValues: []string{"http", "https", "tcp", "websocket"}}, {Name: "outcome", AllowedValues: cloneStrings(requestOutcome)}}},
		{Name: RouteRequests, Kind: Counter, Labels: []LabelDescriptor{{Name: "route_kind", AllowedValues: RouteKinds()}, {Name: "outcome", AllowedValues: cloneStrings(requestOutcome)}}},
		{Name: UpdateOperations, Kind: Counter, Labels: []LabelDescriptor{{Name: "phase", AllowedValues: []string{"download", "verify", "stage", "activate", "health_gate", "rollback", "quarantine"}}, {Name: "outcome", AllowedValues: cloneStrings(operationOutcome)}}},
		{Name: OperationLatency, Kind: Histogram, Labels: []LabelDescriptor{{Name: "operation", AllowedValues: []string{"create", "update", "delete", "reconcile", "certificate", "access"}}}, Histogram: &HistogramDescriptor{Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10}}},
	}
}

// cloneStrings returns an independent copy of values.
func cloneStrings(values []string) []string { return append([]string(nil), values...) }

// PreviewAllocationOutcomes returns the outcome label values of the preview
// allocation counter, one vocabulary for the edge and the control plane.
func PreviewAllocationOutcomes() []string {
	return []string{"success", "failed", "rejected", "canceled", "expired"}
}
