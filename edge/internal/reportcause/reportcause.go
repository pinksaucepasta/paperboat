// Package reportcause registers the tunnel's failures, vocabulary and typed errors with shared/reporting.
package reportcause

import (
	"github.com/getsentry/sentry-go"
	"github.com/pinksaucepasta/paperboat/edge/internal/auth"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgeerrors"
	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
	"reflect"
)

func init() {
	reporting.Register(reporting.Extension{
		Definitions: map[string]reporting.Definition{
			"inspector_stream":         {Operation: "connector_stream", Stage: "inspector_stream", Name: "%s_inspector_stream_failed", Code: "inspector_stream_failed"},
			"browser_terminal_stream":  {Operation: "connector_stream", Stage: "browser_terminal_stream", Name: "%s_browser_terminal_stream_failed", Code: "browser_terminal_stream_failed"},
			"private_stream_open":      {Operation: "connector_stream", Stage: "private_stream_open", Name: "%s_private_stream_open_failed", Code: "private_stream_open_failed"},
			"private_stream_authorize": {Operation: "connector_stream", Stage: "private_stream_authorize", Name: "%s_private_stream_authorize_failed", Code: "private_stream_authorize_failed"},
			"private_stream_target":    {Operation: "connector_stream", Stage: "private_stream_target", Name: "%s_private_stream_target_failed", Code: "private_stream_target_failed"},
			"private_stream_result":    {Operation: "connector_stream", Stage: "private_stream_result", Name: "%s_private_stream_result_failed", Code: "private_stream_result_failed"},
			"private_stream_copy":      {Operation: "connector_stream", Stage: "private_stream_copy", Name: "%s_private_stream_copy_failed", Code: "private_stream_copy_failed"},
			"public_tcp_reconcile":     {Operation: "control_reconcile", Stage: "public_tcp_reconcile", Name: "%s_public_tcp_reconcile_failed", Code: "public_tcp_reconcile_failed"},
			"public_tcp_listener":      {Operation: "service_lifecycle", Stage: "public_tcp_listener", Name: "%s_public_tcp_listener_failed", Code: "public_tcp_listener_failed"},
			"public_tcp_stream":        {Operation: "connector_stream", Stage: "public_tcp_stream", Name: "%s_public_tcp_stream_failed", Code: "public_tcp_stream_failed"},
			"tls_dispatch":             {Operation: "connector_stream", Stage: "tls_dispatch", Name: "%s_tls_dispatch_failed", Code: "tls_dispatch_failed"},
			"control_trust":            {Operation: "control_reconcile", Stage: "control_trust", Name: "%s_control_trust_failed", Code: "control_trust_failed"},
			"carrier_handler":          {Operation: "connector_stream", Stage: "carrier_handler", Name: "%s_carrier_handler_failed", Code: "carrier_handler_failed"},
			"usage_delivery":           {Operation: "usage_delivery", Stage: "usage_delivery", Name: "%s_usage_delivery_failed", Code: "usage_delivery_failed"},
			"node_heartbeat":           {Operation: "control_reconcile", Stage: "node_heartbeat", Name: "%s_node_heartbeat_failed", Code: "node_heartbeat_failed"},
			"http_server":              {Operation: "service_lifecycle", Stage: "http_server", Name: "%s_http_server_failed", Code: "http_server_failed"},
			"runtime_admission":        {Operation: "control_reconcile", Stage: "runtime_admission", Name: "%s_runtime_admission_failed", Code: "runtime_admission_failed"},
			"runtime_observation":      {Operation: "control_reconcile", Stage: "runtime_observation", Name: "%s_runtime_observation_failed", Code: "runtime_observation_failed"},
			"preview_admission":        {Operation: "control_reconcile", Stage: "preview_admission", Name: "%s_preview_admission_failed", Code: "preview_admission_failed"},
			"preview_observation":      {Operation: "control_reconcile", Stage: "preview_observation", Name: "%s_preview_observation_failed", Code: "preview_observation_failed"},
			"certificate_distribution": {Operation: "control_reconcile", Stage: "certificate_distribution", Name: "%s_certificate_distribution_failed", Code: "certificate_distribution_failed"},
			"upstream_request":         {Operation: "connector_stream", Stage: "forward", Name: "%s_upstream_request_failed", Code: "upstream_request_failed"},
			"service_shutdown":         {Operation: "service_lifecycle", Stage: "shutdown", Name: "%s_service_shutdown_failed", Code: "service_shutdown_failed"},
			"route_reconcile":          {Operation: "control_reconcile", Stage: "reconcile", Name: "%s_route_reconcile_failed", Code: "control_reconcile_failed"},
			"route_group_probe":        {Operation: "control_reconcile", Stage: "route_probe", Name: "%s_route_group_unavailable", Code: "route_group_unavailable"},
		},
		Codes:  []string{"stage_accepted", "stage_rejected", "generation_ready", "activated", "drain_started", "drain_completed", "drain_forced", "stale_generation_rejected", "stream_acquired", "stream_overloaded", "stream_released", "control_trust_failed", "carrier_handler_failed", "usage_delivery_failed", "node_heartbeat_failed", "http_server_failed", "runtime_admission_failed", "runtime_observation_failed", "preview_admission_failed", "preview_observation_failed", "certificate_distribution_failed", "upstream_request_failed", "service_shutdown_failed", "control_reconcile_failed", "route_group_unavailable"},
		Causes: []string{"trust_snapshot_invalid", "capacity_exhausted", "control_invalid", "control_unavailable", "node_observation_stale", "route_invalid", "route_state_stale", "route_unavailable", "authority_expired", "authority_rejected", "authorization_rejected", "configuration_invalid", "state_conflict", "authority_unavailable"},
		Stages: map[string]string{
			"control_trust":            "%s_control_trust_failed",
			"carrier_handler":          "%s_carrier_handler_failed",
			"usage_delivery":           "%s_usage_delivery_failed",
			"node_heartbeat":           "%s_node_heartbeat_failed",
			"http_server":              "%s_http_server_failed",
			"runtime_admission":        "%s_runtime_admission_failed",
			"runtime_observation":      "%s_runtime_observation_failed",
			"preview_admission":        "%s_preview_admission_failed",
			"preview_observation":      "%s_preview_observation_failed",
			"certificate_distribution": "%s_certificate_distribution_failed",
			"forward":                  "%s_upstream_request_failed",
			"shutdown":                 "%s_service_shutdown_failed",
			"reconcile":                "%s_route_reconcile_failed",
			"route_probe":              "%s_route_group_unavailable",
		},
		Classify:   classify,
		Reference:  requestReference,
		Lifecycles: map[string]func(code, outcome string) (string, string, bool){"route_lifecycle": routeLifecycle},
		Metrics:    metrics(),
	})
}

func routeLifecycle(code, outcome string) (string, string, bool) {
	input, err := (route.RouteTelemetryRecord{Type: route.LifecycleType(code)}).EventInput()
	if err != nil || string(input.Outcome) != outcome {
		return "", "", false
	}
	switch input.Severity {
	case edgetelemetry.SeverityWarn:
		return string(input.Retry), "warn", true
	case edgetelemetry.SeverityDebug:
		return string(input.Retry), "debug", true
	}
	return string(input.Retry), "", true
}

func metrics() []reporting.Metric {
	var out []reporting.Metric
	for _, descriptor := range edgetelemetry.MetricDescriptors() {
		labels := map[string][]string{}
		for _, label := range descriptor.Labels {
			labels[label.Name] = label.AllowedValues
		}
		switch descriptor.Kind {
		case edgetelemetry.MetricGauge:
			out = append(out, reporting.Metric{Name: descriptor.Name, Kind: sentry.MetricTypeGauge, Labels: labels})
		case edgetelemetry.MetricCounter:
			out = append(out, reporting.Metric{Name: descriptor.Name + "_snapshot", Kind: sentry.MetricTypeGauge, Labels: labels})
		case edgetelemetry.MetricHistogram:
			out = append(out,
				reporting.Metric{Name: descriptor.Name + "_sum_snapshot", Kind: sentry.MetricTypeGauge, Labels: labels},
				reporting.Metric{Name: descriptor.Name + "_count_snapshot", Kind: sentry.MetricTypeGauge, Labels: labels})
		}
	}
	return out
}

func classify(err error) (string, int) {
	if reporting.SameError(err, auth.ErrSnapshotInvalid) {
		return "trust_snapshot_invalid", 87
	}
	if reporting.SameError(err, auth.ErrSnapshotCapacity) {
		return "capacity_exhausted", 86
	}
	if reporting.SameError(err, control.ErrControlInvalid) {
		return "control_invalid", 85
	}
	if reporting.SameError(err, control.ErrControlUnavailable) {
		return "control_unavailable", 40
	}
	if reporting.SameError(err, control.ErrNodeObservationStale) {
		return "node_observation_stale", 83
	}
	if reporting.SameError(err, route.ErrInvalid) {
		return "route_invalid", 82
	}
	if reporting.SameError(err, route.ErrGenerationStale) || reporting.SameError(err, route.ErrGenerationConflict) {
		return "route_state_stale", 81
	}
	if reporting.SameError(err, route.ErrStreamOverloaded) {
		return "capacity_exhausted", 80
	}
	if reporting.SameError(err, route.ErrGenerationClosed) || reporting.SameError(err, route.ErrGenerationNotReady) {
		return "route_unavailable", 79
	}
	if typed, ok := err.(*edgeerrors.Error); ok {
		switch typed.Code {
		case edgeerrors.CodeCredentialExpired, edgeerrors.CodeRunIDExpired:
			return "authority_expired", 78
		case edgeerrors.CodeRevoked, edgeerrors.CodeRunIDRevoked, edgeerrors.CodeGenerationStale:
			return "authority_rejected", 78
		case edgeerrors.CodeCredentialInvalid, edgeerrors.CodeCredentialMalformed, edgeerrors.CodeCredentialSignatureInvalid, edgeerrors.CodeBindingInvalid, edgeerrors.CodeRunIDInvalid, edgeerrors.CodeRunIDMismatch:
			return "authorization_rejected", 78
		case edgeerrors.CodeStoreCapacity:
			return "capacity_exhausted", 77
		case edgeerrors.CodeServiceUnavailable:
			return "service_unavailable", 77
		case edgeerrors.CodeRouteInvalid, edgeerrors.CodeConfigInvalid:
			return "configuration_invalid", 77
		case edgeerrors.CodeRouteConflict, edgeerrors.CodeOperationConflict:
			return "state_conflict", 77
		case edgeerrors.CodeCredentialKeyUnavailable, edgeerrors.CodeCredentialRevocationUnavailable:
			return "authority_unavailable", 77
		}
	}

	return "", 0
}

func requestReference(err error) string {
	remaining := []error{err}
	seen := make(map[error]bool)
	for count := 0; len(remaining) > 0 && count < 16; count++ {
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			continue
		}
		typ := reflect.TypeOf(current)
		if typ != nil && typ.Comparable() {
			if seen[current] {
				continue
			}
			seen[current] = true
		}
		if failure, ok := current.(*control.RequestFailure); ok && failure != nil && failure.SupportReference != "" {
			return failure.SupportReference
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) > 16-count {
				children = children[:16-count]
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			remaining = append(remaining, wrapped.Unwrap())
		}
	}
	return ""
}
