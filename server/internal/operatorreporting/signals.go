package operatorreporting

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
	"slices"
)

var ErrUnsupported = errors.New("operator reporting capability unsupported")

var traceIDPattern = regexpMust(`^[0-9a-f]{32}$`)
var spanIDPattern = regexpMust(`^[0-9a-f]{16}$`)

// regexpMust is kept local so all provider-derived strings still pass through
// the stricter safeToken projection before disclosure.
func regexpMust(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

var periods = map[string]int{"1h": 3600, "6h": 6 * 3600, "24h": 24 * 3600, "7d": 7 * 24 * 3600}
var components = map[string]bool{"paperboat-server": true, "paperboat-cli": true, "paperboat-daemon": true, "paperboat-relay": true, "paperboat-tunnel": true}
var semanticOperations = tokenSet(`http_auth http_accounts http_teams http_billing http_admin http_machines http_terminal http_environment http_config http_files http_tunnels http_previews http_releases http_edge http_network http_support http_other http_health provider_workos provider_stripe provider_github provider_provider_route provider_helper provider_unknown dns_verification certificate_lifecycle certificate_expiry preview_allocation preview_cleanup connector_session connector_connection connector_reconnect connector_backoff connector_handshake connector_disconnect config_apply route_error route_latency service_lifecycle service_watchdog service_crash_loop http_request health_transition worker_operation backup_operation database_operation worker_machines worker_config_warnings worker_config_revocation worker_preview_leases worker_tunnel_expiry worker_connector_rotation worker_connector_drain worker_domain_dns worker_preview_dns worker_preview_outbox worker_team_retention worker_transactional_mail worker_team_entitlements worker_team_billing worker_team_inbox worker_edge_assignments worker_machine_events worker_edge_sets worker_peer_endpoints worker_stale_nodes worker_edge_dns worker_diagnostic_uploads worker_certificates worker_platform_certificates machine_pairing preview_observation private_authorization preview_attachment runtime_observation authorization_keys browser_authorization revocation_refresh accessor_discovery helper_enrollment config_sync identity_renewal machine_control peer_identity environment_enrollment tunnel_enrollment connector_admission tunnel_bootstrap inspector_authorization component_start component_shutdown component_rollback route_transition exec sftp rsync environments team desktop ping relay auth inbox session sessions machine pair setup uninstall install send transfer wait bugreport resolve tag approve service edge route origin dns certificate access login logout machine project workspace connect terminal ssh scp serve preview tunnel env vault config sync update doctor status version help daemon command control_request runtime_lifecycle diagnostic relay_admission relay_session relay_service dependency_health control_reconcile connector_attach connector_stream usage_delivery capacity backlog`)
var semanticOutcomes = tokenSet(`success failed rejected canceled state_change`)
var semanticStages = tokenSet(`http_handler panic service authorize retry handler worker_machines worker_config_warnings worker_config_revocation worker_preview_leases worker_tunnel_expiry worker_connector_rotation worker_connector_drain worker_domain_dns worker_preview_dns worker_preview_outbox worker_team_retention worker_transactional_mail worker_team_entitlements worker_team_billing worker_team_inbox worker_edge_assignments worker_machine_events worker_edge_sets worker_peer_endpoints worker_stale_nodes worker_edge_dns worker_diagnostic_uploads worker_certificates worker_platform_certificates`)
var semanticCodes = tokenSet(`internal_error timeout panic service_failure control_sync_failed http_server_failed worker_failed worker_panic operation_succeeded operation_failed operation_rejected operation_canceled state_changed ready stopped start_failed start_canceled start_deadline shutdown_failed shutdown_canceled shutdown_deadline rollback_failed rollback_canceled rollback_deadline route_state route_stale lifecycle_state ok internal invalid unauthorized capacity unavailable shutdown recovered`)
var semanticDimensions = tokenSet(`service edge config route origin dns certificate access update`)

func tokenSet(values string) map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Fields(values) {
		out[v] = true
	}
	return out
}
func safeSemantic(value string, allowed map[string]bool) string {
	value = boundedToken(value, 128)
	if !allowed[value] {
		return ""
	}
	return value
}

type LogEntry struct {
	Region           string    `json:"region,omitempty"`
	Instance         string    `json:"instance,omitempty"`
	ID               string    `json:"id,omitempty"`
	Project          string    `json:"project"`
	Timestamp        time.Time `json:"timestamp"`
	Severity         string    `json:"severity,omitempty"`
	TraceID          string    `json:"trace_id,omitempty"`
	SupportReference string    `json:"support_reference,omitempty"`
	Component        string    `json:"component,omitempty"`
	Operation        string    `json:"operation,omitempty"`
	Outcome          string    `json:"outcome,omitempty"`
	Dimension        string    `json:"dimension,omitempty"`
	Stage            string    `json:"stage,omitempty"`
	Code             string    `json:"code,omitempty"`
}
type LogPage struct {
	Logs       []LogEntry `json:"logs"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type CorrelatedSignal struct {
	Region    string    `json:"region,omitempty"`
	Instance  string    `json:"instance,omitempty"`
	Dataset   string    `json:"dataset"`
	ID        string    `json:"id,omitempty"`
	Project   string    `json:"project"`
	Timestamp time.Time `json:"timestamp"`
	TraceID   string    `json:"trace_id,omitempty"`
	Component string    `json:"component,omitempty"`
	Operation string    `json:"operation,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
	Code      string    `json:"code,omitempty"`
	Dimension string    `json:"dimension,omitempty"`
}
type ReferenceSignals struct {
	SupportReference string             `json:"support_reference"`
	Signals          []CorrelatedSignal `json:"signals"`
	Datasets         map[string]string  `json:"datasets"`
	Truncated        bool               `json:"truncated"`
}

type exploreResponse struct {
	Data []map[string]json.RawMessage `json:"data"`
}

func (s *Service) ListLogs(ctx context.Context, actor auth.User, period, cursor, reference, component string, limit int) (LogPage, error) {
	out := LogPage{Logs: []LogEntry{}}
	if periods[period] == 0 || !validCursor(cursor) || limit < 1 || limit > 100 || reference != "" && !supportreport.ValidReference(reference) || component != "" && !components[component] {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	projects, err := s.projectDetails(ctx)
	if err != nil {
		return out, err
	}
	q := url.Values{"dataset": {"logs"}, "statsPeriod": {period}, "per_page": {strconv.Itoa(limit)}, "sort": {"-timestamp"}, "field": {"id", "timestamp", "severity", "project", "trace", "component", "operation", "outcome", "dimension", "stage", "code", "support_reference", "region", "instance"}}
	for _, p := range projects {
		q.Add("project", p.ID)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if reference != "" {
		q.Set("query", "support_reference:"+reference)
	}
	if component != "" {
		q.Set("query", joinFixedQuery(q.Get("query"), "component:"+component))
	}
	var raw exploreResponse
	header, err := s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/events/", q, &raw)
	if err != nil {
		if reportingErrorOnly(err, ErrNotFound, errProviderRejected) {
			return out, ErrUnsupported
		}
		return out, err
	}
	allowed := projectMap(projects)
	for _, row := range raw.Data {
		entry, ok := safeLog(row, allowed)
		if !ok {
			return LogPage{}, ErrUnavailable
		}
		if reference != "" && entry.SupportReference != reference {
			return LogPage{}, ErrUnavailable
		}
		out.Logs = append(out.Logs, entry)
	}
	if len(out.Logs) > limit {
		out.Logs = out.Logs[:limit]
	}
	out.NextCursor = nextCursor(header.Get("Link"))
	if err := s.auditRemote(ctx, actor, "sentry_logs", reference, len(out.Logs)); err != nil {
		return LogPage{}, err
	}
	return out, nil
}

func safeLog(row map[string]json.RawMessage, allowed map[string]string) (LogEntry, bool) {
	str := func(key string) string { var v string; _ = json.Unmarshal(row[key], &v); return v }
	project := str("project")
	if project == "" {
		project = str("project.name")
	}
	if _, ok := allowed[project]; !ok {
		return LogEntry{}, false
	}
	var at time.Time
	if err := json.Unmarshal(row["timestamp"], &at); err != nil || at.IsZero() {
		return LogEntry{}, false
	}
	region, instance := safeFleet(str("region"), str("instance"))
	e := LogEntry{Region: region, Instance: instance, ID: boundedToken(str("id"), 128), Project: project, Timestamp: at, Severity: boundedToken(str("severity"), 32), TraceID: boundedToken(str("trace"), 64), Component: boundedToken(str("component"), 128), Operation: safeSemantic(str("operation"), semanticOperations), Outcome: safeSemantic(str("outcome"), semanticOutcomes), Dimension: safeSemantic(str("dimension"), semanticDimensions), Stage: safeSemantic(str("stage"), semanticStages), Code: safeSemantic(str("code"), semanticCodes)}
	if e.Component != "" && !components[e.Component] {
		return LogEntry{}, false
	}
	if ref := str("support_reference"); supportreport.ValidReference(ref) {
		e.SupportReference = ref
	}
	return e, true
}

// CorrelateReference searches provider evidence directly. The reference is a
// diagnostic filter only; staff authorization and project allowlisting remain
// the disclosure boundary.
func (s *Service) CorrelateReference(ctx context.Context, actor auth.User, reference, period string, limit int) (ReferenceSignals, error) {
	out := ReferenceSignals{SupportReference: reference, Signals: []CorrelatedSignal{}, Datasets: map[string]string{"errors": "unavailable", "logs": "unavailable", "spans": "unavailable"}}
	if !supportreport.ValidReference(reference) || periods[period] == 0 || limit < 1 || limit > 100 {
		return ReferenceSignals{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return ReferenceSignals{}, err
	}
	projects, err := s.projectDetails(ctx)
	if err != nil {
		return ReferenceSignals{}, err
	}
	allowed := projectMap(projects)
	for _, dataset := range []string{"errors", "logs", "spans"} {
		q := url.Values{"dataset": {dataset}, "statsPeriod": {period}, "per_page": {strconv.Itoa(limit)}, "sort": {"-timestamp"}, "query": {"support_reference:" + reference}, "field": {"id", "timestamp", "project", "trace", "component", "operation", "outcome", "dimension", "code", "support_reference", "region", "instance"}}
		for _, p := range projects {
			q.Add("project", p.ID)
		}
		var raw exploreResponse
		if _, err = s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/events/", q, &raw); err != nil {
			if reportingErrorOnly(err, ErrNotFound, errProviderRejected) {
				out.Datasets[dataset] = "unsupported"
				continue
			}
			return ReferenceSignals{}, err
		}
		out.Datasets[dataset] = "available"
		for _, row := range raw.Data {
			if len(out.Signals) >= limit {
				out.Truncated = true
				break
			}
			str := func(k string) string { var v string; _ = json.Unmarshal(row[k], &v); return v }
			project := str("project")
			if _, ok := allowed[project]; !ok {
				return ReferenceSignals{}, ErrUnavailable
			}
			if str("support_reference") != reference {
				return ReferenceSignals{}, ErrUnavailable
			}
			var at time.Time
			if json.Unmarshal(row["timestamp"], &at) != nil || at.IsZero() {
				return ReferenceSignals{}, ErrUnavailable
			}
			component := boundedToken(str("component"), 128)
			if component != "" && !components[component] {
				return ReferenceSignals{}, ErrUnavailable
			}
			region, instance := safeFleet(str("region"), str("instance"))
			out.Signals = append(out.Signals, CorrelatedSignal{Region: region, Instance: instance, Dataset: dataset, ID: boundedToken(str("id"), 128), Project: project, Timestamp: at, TraceID: boundedToken(str("trace"), 64), Component: component, Operation: safeSemantic(str("operation"), semanticOperations), Outcome: safeSemantic(str("outcome"), semanticOutcomes), Dimension: safeSemantic(str("dimension"), semanticDimensions), Code: safeSemantic(str("code"), semanticCodes)})
		}
	}
	if err = s.auditRemote(ctx, actor, "sentry_reference_signals", reference, len(out.Signals)); err != nil {
		return ReferenceSignals{}, err
	}
	return out, nil
}

func joinFixedQuery(left, right string) string {
	if left == "" {
		return right
	}
	return left + " " + right
}

type TraceItem struct {
	EventID       string    `json:"event_id"`
	ParentEventID string    `json:"parent_event_id,omitempty"`
	Project       string    `json:"project"`
	Type          string    `json:"type"`
	Operation     string    `json:"operation,omitempty"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	DurationMS    float64   `json:"duration_ms,omitempty"`
}
type Trace struct {
	TraceID   string      `json:"trace_id"`
	Items     []TraceItem `json:"items"`
	Truncated bool        `json:"truncated"`
}
type rawTraceItem struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	ProjectID     json.RawMessage `json:"project_id"`
	ProjectSlug   string          `json:"project_slug"`
	Op            string          `json:"op"`
	Name          string          `json:"name"`
	Start         float64         `json:"start_timestamp"`
	End           float64         `json:"end_timestamp"`
	Duration      float64         `json:"duration"`
	IsTransaction bool            `json:"is_transaction"`
	Children      []rawTraceItem  `json:"children"`
	Errors        []rawTraceItem  `json:"errors"`
	Occurrences   []rawTraceItem  `json:"occurrences"`
}

func (s *Service) GetTrace(ctx context.Context, actor auth.User, traceID, period string) (Trace, error) {
	out := Trace{TraceID: traceID, Items: []TraceItem{}}
	if !traceIDPattern.MatchString(traceID) || periods[period] == 0 {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	projects, err := s.projectDetails(ctx)
	if err != nil {
		return out, err
	}
	var raw []rawTraceItem
	if _, err = s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/trace/"+traceID+"/", url.Values{"statsPeriod": {period}}, &raw); err != nil {
		return out, err
	}
	ids := map[string]string{}
	for _, p := range projects {
		ids[p.ID] = p.Slug
	}
	var walk func([]rawTraceItem, string) error
	walk = func(items []rawTraceItem, parent string) error {
		for _, v := range items {
			if len(out.Items) >= 200 {
				out.Truncated = true
				continue
			}
			item, ok := safeTraceItem(v, ids, parent)
			if !ok {
				return ErrUnavailable
			}
			out.Items = append(out.Items, item)
			if err := walk(v.Children, v.EventID); err != nil {
				return err
			}
			if err := walk(v.Errors, v.EventID); err != nil {
				return err
			}
			if err := walk(v.Occurrences, v.EventID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(raw, ""); err != nil {
		return Trace{}, err
	}
	if err := s.auditRemote(ctx, actor, "sentry_trace", traceID, len(out.Items)); err != nil {
		return Trace{}, err
	}
	return out, nil
}

func safeTraceItem(v rawTraceItem, projects map[string]string, parent string) (TraceItem, bool) {
	slug, projectOK := projects[rawID(v.ProjectID)]
	idOK := v.EventType == "span" && (spanIDPattern.MatchString(v.EventID) || v.IsTransaction && traceIDPattern.MatchString(v.EventID)) || (v.EventType == "error" || v.EventType == "occurrence") && traceIDPattern.MatchString(v.EventID)
	if (v.EventType == "error" || v.EventType == "occurrence") && v.End == 0 {
		v.End = v.Start
	}
	start, startOK := traceTimestamp(v.Start)
	end, endOK := traceTimestamp(v.End)
	if !projectOK || v.ProjectSlug != slug || !idOK || !startOK || !endOK || v.End < v.Start || v.Duration < 0 || math.IsInf(v.Duration, 0) || math.IsNaN(v.Duration) {
		return TraceItem{}, false
	}
	op := safeSemantic(v.Op, semanticOperations)
	if op == "" {
		op = safeSemantic(v.Name, semanticOperations)
	}
	return TraceItem{EventID: v.EventID, ParentEventID: parent, Project: slug, Type: v.EventType, Operation: op, Start: start, End: end, DurationMS: v.Duration}, true
}

func traceTimestamp(value float64) (time.Time, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 946684800 || value > 4102444800 {
		return time.Time{}, false
	}
	seconds, fraction := math.Modf(value)
	return time.Unix(int64(seconds), int64(math.Round(fraction*1e9))).UTC(), true
}

func rawID(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(v, &n) == nil {
		return n.String()
	}
	return ""
}

type MetricPoint struct {
	Timestamp  time.Time `json:"timestamp"`
	Value      float64   `json:"value"`
	Incomplete bool      `json:"incomplete,omitempty"`
}
type MetricSeries struct {
	Worker             string        `json:"worker,omitempty"`
	Region             string        `json:"region,omitempty"`
	Instance           string        `json:"instance,omitempty"`
	Scope              string        `json:"scope"`
	Semantics          string        `json:"semantics"`
	Freshness          string        `json:"freshness"`
	IntervalSeconds    int           `json:"interval_seconds"`
	LastObservedBucket *time.Time    `json:"last_observed_bucket,omitempty"`
	Dataset            string        `json:"dataset"`
	Component          string        `json:"component"`
	Aggregation        string        `json:"aggregation"`
	Points             []MetricPoint `json:"points"`
}
type MetricDefinition struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Unit     string    `json:"unit,omitempty"`
	Count    int64     `json:"count"`
	LastSeen time.Time `json:"last_seen"`
}
type MetricCatalog struct {
	Metrics []MetricDefinition `json:"metrics"`
}
type rawMetricDefinition struct {
	Name, Type string
	Unit       *string
	Count      float64
	LastSeen   float64
}

func (s *Service) MetricCatalog(ctx context.Context, actor auth.User, period string) (MetricCatalog, error) {
	out := MetricCatalog{Metrics: []MetricDefinition{}}
	if periods[period] == 0 {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	projects, err := s.projectDetails(ctx)
	if err != nil {
		return out, err
	}
	q := url.Values{"statsPeriod": {period}, "sort": {"name"}}
	for _, p := range projects {
		q.Add("project", p.ID)
	}
	var raw []rawMetricDefinition
	if _, err = s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/trace-items/metrics/", q, &raw); err != nil {
		if reportingErrorOnly(err, ErrNotFound, errProviderRejected) {
			return out, ErrUnsupported
		}
		return out, err
	}
	if len(raw) > 200 {
		return MetricCatalog{}, ErrUnavailable
	}
	allowedMetrics := metricAllowlist()
	for _, v := range raw {
		spec, allowed := allowedMetrics[v.Name]
		if !allowed {
			continue
		}
		if v.Type != spec.Kind || v.Count < 0 || v.Count > 1e12 || math.Trunc(v.Count) != v.Count || math.IsNaN(v.Count) || math.IsInf(v.Count, 0) {
			return MetricCatalog{}, ErrUnavailable
		}
		lastSeen, ok := metricLastSeen(v.LastSeen)
		if !ok {
			return MetricCatalog{}, ErrUnavailable
		}
		unit := spec.Unit
		if v.Unit != nil && boundedToken(*v.Unit, 32) != unit {
			return MetricCatalog{}, ErrUnavailable
		}
		out.Metrics = append(out.Metrics, MetricDefinition{Name: v.Name, Type: spec.Kind, Unit: unit, Count: int64(v.Count), LastSeen: lastSeen})
	}
	if err = s.auditRemote(ctx, actor, "sentry_metric_catalog", period, len(out.Metrics)); err != nil {
		return MetricCatalog{}, err
	}
	return out, nil
}

func metricLastSeen(value float64) (time.Time, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 946684800e9 || value > 4102444800e9 {
		return time.Time{}, false
	}
	nanos := int64(value)
	return time.Unix(0, nanos).UTC(), true
}

type metricSpec struct{ Kind, Unit, Aggregation string }

func metricAllowlist() map[string]metricSpec {
	out := map[string]metricSpec{
		"paperboat.operation.count":    {Kind: "counter", Unit: "none", Aggregation: "sum(value,paperboat.operation.count,counter,none)"},
		"paperboat.operation.duration": {Kind: "distribution", Unit: "second", Aggregation: "p95(value,paperboat.operation.duration,distribution,second)"},
	}
	addGauge := func(name string) {
		out[name] = metricSpec{Kind: "gauge", Unit: "none", Aggregation: "max(value," + name + ",gauge,none)"}
	}
	for _, d := range telemetry.MetricDescriptors() {
		switch d.Kind {
		case telemetry.MetricCounter:
			addGauge(d.Name + "_snapshot")
		case telemetry.MetricHistogram:
			addGauge(d.Name + "_sum_snapshot")
			addGauge(d.Name + "_count_snapshot")
		case telemetry.MetricGauge:
			addGauge(d.Name)
		}
	}
	for _, name := range []string{
		"paperboat_runtime_service_uptime_seconds", "paperboat_runtime_service_restarts_total_snapshot", "paperboat_runtime_watchdog_failures_total_snapshot", "paperboat_runtime_crash_loop", "paperboat_runtime_health_dimension", "paperboat_runtime_health_transitions_total_snapshot", "paperboat_runtime_update_operations_total_snapshot", "paperboat_runtime_doctor_checks_total_snapshot", "paperboat_runtime_health_probe_latency_seconds_sum_snapshot", "paperboat_runtime_health_probe_latency_seconds_count_snapshot", "paperboat_runtime_state_evidence",
		"paperboat_edge_config_desired_generation", "paperboat_edge_config_applied_generation", "paperboat_edge_origin_connect_failures_total_snapshot", "paperboat_edge_domain_verification_seconds_sum_snapshot", "paperboat_edge_domain_verification_seconds_count_snapshot", "paperboat_edge_domain_verification_failures_total_snapshot", "paperboat_edge_certificate_failures_total_snapshot", "paperboat_edge_operation_conflicts_total_snapshot",
	} {
		addGauge(name)
	}
	for _, name := range []string{"paperboat_reporting_signal_enabled", "paperboat_relay_sessions", "paperboat_relay_capacity_limit", "paperboat_relay_admissions_total_snapshot", "paperboat_relay_packets_total_snapshot", "paperboat_peer_relay_allocations", "paperboat_peer_relay_admissions_total_snapshot", "paperboat_peer_relay_packets_total_snapshot", "paperboat_reporting_errors_dropped_total_snapshot", "paperboat_reporting_logs_dropped_total_snapshot", "paperboat_reporting_metrics_dropped_total_snapshot", "paperboat_reporting_traces_dropped_total_snapshot"} {
		addGauge(name)
	}
	for _, name := range []string{"machine_requested_total", "machine_completed_total", "machine_login_latency_ms_total", "machine_login_latency_ms_max", "connect_attempts_total", "connect_approved_total", "connect_denied_total", "route_ready_total", "credentials_minted_total", "revocations_propagated_total", "terminal_sessions_created_total", "terminal_sessions_closed_total", "terminal_sessions_deleted_total", "terminal_operations_applied_total", "terminal_operation_retries_total", "terminal_operation_alerts_total", "terminal_snapshots_total", "terminal_snapshot_failures_total", "control_route_observations_total", "control_route_detaches_total", "control_node_stale_total", "control_usage_receipts_total", "control_credentials_issued_total", "control_revocation_fetches_total"} {
		addGauge("paperboat_server_" + name + "_snapshot")
	}
	for _, provider := range []string{"workos", "stripe", "github", "provider_route", "helper", "unknown"} {
		for _, suffix := range []string{"requests_total", "errors_total", "latency_ms_total", "latency_ms_max"} {
			addGauge("paperboat_server_provider_" + provider + "_" + suffix + "_snapshot")
		}
	}
	for _, name := range []string{"paperboat.worker.state", "paperboat.worker.progress_age_seconds", "paperboat.backup.state", "paperboat.restore.state", "paperboat.backup.age_seconds", "paperboat.restore.age_seconds", "paperboat.database.connections", "paperboat.database.acquired", "paperboat.database.idle", "paperboat.database.acquire_wait_seconds_snapshot", "paperboat.database.acquire_canceled_snapshot", "paperboat.runtime.goroutines", "paperboat.runtime.heap_bytes", "paperboat.reporting.dropped"} {
		addGauge(name)
	}
	return out
}

type rawTimeSeries struct {
	TimeSeries []struct {
		Values []struct {
			Timestamp   int64    `json:"timestamp"`
			Value       *float64 `json:"value"`
			SampleCount float64  `json:"sampleCount"`
			Incomplete  bool     `json:"incomplete"`
		} `json:"values"`
		YAxis   string                        `json:"yAxis"`
		GroupBy []struct{ Key, Value string } `json:"groupBy"`
	} `json:"timeSeries"`
}

func (s *Service) Metrics(ctx context.Context, actor auth.User, metric, component, period string, fleet ...FleetFilter) (MetricSeries, error) {
	aggregation := metricAllowlist()[metric].Aggregation
	out := MetricSeries{Dataset: metric, Component: component, Aggregation: aggregation, Points: []MetricPoint{}, Scope: "component_population", Semantics: "interval_population_maximum", Freshness: "unknown", IntervalSeconds: metricInterval(period)}
	if metric == "paperboat.operation.count" {
		out.Semantics = "interval_exported_operation_sum"
	}
	if metric == "paperboat.operation.duration" {
		out.Semantics = "interval_exported_operation_p95"
	}
	if len(fleet) > 1 {
		return out, ErrInvalid
	}
	if len(fleet) == 1 {
		f := fleet[0]
		if f.Worker != "" {
			if !slices.Contains(workers.ProgressNames(), f.Worker) || metric != "paperboat.worker.state" && metric != "paperboat.worker.progress_age_seconds" {
				return out, ErrInvalid
			}
			out.Worker = f.Worker
		}
		if !validFleet(f.Region, f.Instance) || (f.Region != "" && (component == "paperboat-cli" || component == "paperboat-daemon")) {
			return out, ErrInvalid
		}
		out.Region, out.Instance = f.Region, f.Instance
		if f.Region != "" {
			out.Scope = "deployment_slot"
			if metric != "paperboat.operation.count" && metric != "paperboat.operation.duration" {
				out.Semantics = "interval_slot_maximum"
			}
		}
	}
	if aggregation == "" || !components[component] || periods[period] == 0 {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := s.preAuthorize(ctx, actor); err != nil {
		return out, err
	}
	projects, err := s.projectDetails(ctx)
	if err != nil {
		return out, err
	}
	q := url.Values{"dataset": {"tracemetrics"}, "statsPeriod": {period}, "yAxis": {aggregation}, "interval": {strconv.Itoa(metricInterval(period))}, "query": {"component:" + component}}
	if out.Worker != "" {
		q.Set("query", q.Get("query")+" worker:"+out.Worker)
	}
	if out.Region != "" {
		q.Set("query", q.Get("query")+" region:"+out.Region+" instance:"+out.Instance)
	}
	for _, p := range projects {
		q.Add("project", p.ID)
	}
	var raw rawTimeSeries
	if _, err = s.get(ctx, "/api/0/organizations/"+url.PathEscape(s.org)+"/events-timeseries/", q, &raw); err != nil {
		if reportingErrorOnly(err, ErrNotFound, errProviderRejected) {
			return out, ErrUnsupported
		}
		return out, err
	}
	if len(raw.TimeSeries) != 1 || raw.TimeSeries[0].YAxis != aggregation || len(raw.TimeSeries[0].GroupBy) != 0 || len(raw.TimeSeries[0].Values) > 169 {
		return MetricSeries{}, ErrUnavailable
	}
	for _, p := range raw.TimeSeries[0].Values {
		if p.Timestamp <= 0 || p.Timestamp > 1e14 || p.SampleCount < 0 || p.SampleCount > 1e12 || math.IsNaN(p.SampleCount) || math.IsInf(p.SampleCount, 0) || math.Trunc(p.SampleCount) != p.SampleCount {
			return MetricSeries{}, ErrUnavailable
		}
		// Sentry fills empty buckets with zero. Absence is unknown, whereas an
		// observed zero with a positive sample count remains a real data point.
		if p.SampleCount == 0 || p.Value == nil {
			continue
		}
		if math.IsInf(*p.Value, 0) || math.IsNaN(*p.Value) {
			return MetricSeries{}, ErrUnavailable
		}
		at := time.UnixMilli(p.Timestamp).UTC()
		if out.LastObservedBucket == nil || at.After(*out.LastObservedBucket) {
			out.LastObservedBucket = &at
		}
		out.Points = append(out.Points, MetricPoint{Timestamp: time.UnixMilli(p.Timestamp).UTC(), Value: *p.Value, Incomplete: p.Incomplete})
	}
	if err = s.auditRemote(ctx, actor, "sentry_metrics", metric, len(out.Points)); err != nil {
		return MetricSeries{}, err
	}
	return out, nil
}
func metricInterval(period string) int {
	if period == "7d" {
		return 3600
	}
	if period == "24h" {
		return 900
	}
	return 300
}
func projectMap(projects []sentryProject) map[string]string {
	out := map[string]string{}
	for _, p := range projects {
		out[p.Slug] = p.ID
	}
	return out
}

// FleetFilter selects one operator-assigned public deployment slot. Native
// processes deliberately do not expose a persistent machine identity.
type FleetFilter struct{ Region, Instance, Worker string }

var fleetRegionPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
var fleetInstancePattern = regexp.MustCompile(`^slot-(0[1-9]|[1-5][0-9]|6[0-4])$`)

func validFleet(region, instance string) bool {
	return region == "" && instance == "" || fleetRegionPattern.MatchString(region) && fleetInstancePattern.MatchString(instance)
}
func safeFleet(region, instance string) (string, string) {
	if validFleet(region, instance) {
		return region, instance
	}
	return "", ""
}
