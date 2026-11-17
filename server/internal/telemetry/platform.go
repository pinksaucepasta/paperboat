package telemetry

import (
	"sort"

	sharedmetrics "github.com/pinksaucepasta/paperboat/shared/metrics"
)

// PlatformProviders are the provider labels of the provider_* platform metrics;
// every other provider is counted as "unknown".
var PlatformProviders = []string{"workos", "stripe", "github", "provider_route", "helper", "unknown"}

// platformMetrics are the control plane's flat platform counters, exposed under
// these exact names by the localhost /metrics endpoint and the Sentry snapshot.
// They live in the same registry as the typed catalog (one instance), unlabeled.
var platformMetrics = func() []sharedmetrics.Descriptor {
	var descriptors []sharedmetrics.Descriptor
	for _, name := range []string{
		"device_requested_total", "device_completed_total", "device_login_latency_ms_total",
		"connect_attempts_total", "connect_approved_total", "connect_denied_total", "route_ready_total",
		"credentials_minted_total", "revocations_propagated_total",
		"terminal_sessions_created_total", "terminal_sessions_closed_total", "terminal_sessions_deleted_total",
		"terminal_operations_applied_total", "terminal_operation_retries_total", "terminal_operation_alerts_total",
		"terminal_snapshots_total", "terminal_snapshot_failures_total",
		"control_route_observations_total", "control_route_detaches_total", "control_node_stale_total",
		"control_usage_receipts_total", "control_credentials_issued_total", "control_revocation_fetches_total",
	} {
		descriptors = append(descriptors, sharedmetrics.Descriptor{Name: name, Kind: sharedmetrics.Counter})
	}
	descriptors = append(descriptors, sharedmetrics.Descriptor{Name: "device_login_latency_ms_max", Kind: sharedmetrics.Gauge})
	for _, provider := range PlatformProviders {
		prefix := "provider_" + provider
		descriptors = append(descriptors,
			sharedmetrics.Descriptor{Name: prefix + "_requests_total", Kind: sharedmetrics.Counter},
			sharedmetrics.Descriptor{Name: prefix + "_errors_total", Kind: sharedmetrics.Counter},
			sharedmetrics.Descriptor{Name: prefix + "_latency_ms_total", Kind: sharedmetrics.Counter},
			sharedmetrics.Descriptor{Name: prefix + "_latency_ms_max", Kind: sharedmetrics.Gauge},
		)
	}
	return descriptors
}()

// PlatformMetricDescriptors returns the flat platform metrics.
func PlatformMetricDescriptors() []sharedmetrics.Descriptor {
	return sharedmetrics.Clone(platformMetrics)
}

// PlatformMetricNames returns the sorted names of the flat platform metrics.
func PlatformMetricNames() []string {
	names := make([]string, len(platformMetrics))
	for index, descriptor := range platformMetrics {
		names[index] = descriptor.Name
	}
	sort.Strings(names)
	return names
}

// IsPlatformMetric reports whether name is a flat platform metric.
func IsPlatformMetric(name string) bool {
	for _, descriptor := range platformMetrics {
		if descriptor.Name == name {
			return true
		}
	}
	return false
}

// TypedSamples is metrics.Snapshot() without the flat platform series, which are
// exposed on their own under their flat names.
func TypedSamples(m *sharedmetrics.Metrics) []sharedmetrics.Sample {
	samples := m.Snapshot()
	typed := samples[:0:0]
	for _, sample := range samples {
		if !IsPlatformMetric(sample.Name) {
			typed = append(typed, sample)
		}
	}
	return typed
}
