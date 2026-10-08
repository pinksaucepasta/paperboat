package observability

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/edgeerrors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/node"
	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/usage"
)

type Kind string
type Result string

const (
	Admission Kind = "admission"
	Route     Kind = "route"
	Stream    Kind = "stream"
	Usage     Kind = "usage"
	Node      Kind = "node"
	Cleanup   Kind = "cleanup"

	Success  Result = "success"
	Rejected Result = "rejected"
	Failed   Result = "failed"
	Canceled Result = "canceled"
)

type Event struct {
	At                  time.Time `json:"at"`
	Kind                Kind      `json:"kind"`
	Result              Result    `json:"result"`
	NodeState           string    `json:"node_state,omitempty"`
	RouteKind           string    `json:"route_kind,omitempty"`
	Direction           string    `json:"direction,omitempty"`
	RejectionCode       string    `json:"rejection_code,omitempty"`
	ConnectorGeneration uint64    `json:"connector_generation,omitempty"`
	RouteRevision       uint64    `json:"route_revision,omitempty"`
	Bytes               uint64    `json:"bytes,omitempty"`
	Streams             uint32    `json:"streams,omitempty"`
}

func (e Event) JSON() ([]byte, error) { return json.Marshal(e) }

type SafeError struct {
	Code     string `json:"code"`
	Recovery string `json:"recovery,omitempty"`
}

func Error(err error) SafeError {
	if code, ok := edgeerrors.CodeOf(err); ok {
		recovery := "retry or inspect private diagnostics"
		switch code {
		case edgeerrors.CodeCredentialReplayed, edgeerrors.CodeCredentialInvalid, edgeerrors.CodeCredentialMalformed, edgeerrors.CodeCredentialSignatureInvalid, edgeerrors.CodeCredentialExpired, edgeerrors.CodeCredentialNotYetValid, edgeerrors.CodeBindingInvalid, edgeerrors.CodeGenerationStale, edgeerrors.CodeRevoked, edgeerrors.CodeRunIDInvalid, edgeerrors.CodeRunIDMismatch, edgeerrors.CodeRunIDExpired, edgeerrors.CodeRunIDRevoked:
			recovery = "request a fresh admission"
		case edgeerrors.CodeCredentialKeyUnavailable, edgeerrors.CodeCredentialRevocationUnavailable, edgeerrors.CodeServiceUnavailable:
			recovery = "retry after control state recovers"
		case edgeerrors.CodeStoreCapacity:
			recovery = "restore journal capacity before retrying"
		case edgeerrors.CodeConfigInvalid, edgeerrors.CodeRouteInvalid:
			recovery = "correct the configuration and retry"
		case edgeerrors.CodeOperationConflict, edgeerrors.CodeRouteConflict, edgeerrors.CodeRouteRevisionStale:
			recovery = "refresh current state and retry"
		}
		return SafeError{Code: string(code), Recovery: recovery}
	}
	return SafeError{Code: "internal_error", Recovery: "retry or inspect private diagnostics"}
}

type MetricDescriptor struct {
	Name    string
	Kind    string
	Labels  map[string][]string
	Buckets []float64
}

func MetricDescriptors() []MetricDescriptor {
	result := []MetricDescriptor{
		{Name: "paperboat_edge_telemetry_dropped_total", Kind: "counter"},
		{Name: "paperboat_tunnel_active_streams", Kind: "gauge"},
		{Name: "paperboat_tunnel_attached_routes", Kind: "gauge"},
		{Name: "paperboat_tunnel_connector_capacity", Kind: "gauge"},
		{Name: "paperboat_tunnel_connectors", Kind: "gauge"},
		{Name: "paperboat_tunnel_dependency_healthy", Kind: "gauge", Labels: map[string][]string{"dependency": {"carrier", "control", "routes", "usage"}}},
		{Name: "paperboat_tunnel_live", Kind: "gauge"},
		{Name: "paperboat_tunnel_ready", Kind: "gauge"},
		{Name: "paperboat_tunnel_traffic_egress_bytes_total", Kind: "counter"},
		{Name: "paperboat_tunnel_traffic_ingress_bytes_total", Kind: "counter"},
		{Name: "paperboat_tunnel_usage_oldest_age_seconds", Kind: "gauge"},
		{Name: "paperboat_tunnel_usage_pending_bytes", Kind: "gauge"},
		{Name: "paperboat_tunnel_usage_pending_reports", Kind: "gauge"},
	}
	for _, descriptor := range edgetelemetry.MetricDescriptors() {
		labels := make(map[string][]string, len(descriptor.Labels))
		var buckets []float64
		for _, label := range descriptor.Labels {
			values := append([]string(nil), label.AllowedValues...)
			sort.Strings(values)
			labels[label.Name] = values
		}
		if descriptor.Histogram != nil {
			buckets = append([]float64(nil), descriptor.Histogram.Buckets...)
		}
		result = append(result, MetricDescriptor{
			Name: descriptor.Name, Kind: string(descriptor.Kind), Labels: labels,
			Buckets: buckets,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

type Status string

const (
	Healthy     Status = "healthy"
	Degraded    Status = "degraded"
	Unavailable Status = "unavailable"
)

type Diagnostics struct {
	Control Status `json:"control"`
	Store   Status `json:"store"`
	Carrier Status `json:"carrier"`
	Usage   Status `json:"usage"`
}

func (d Diagnostics) Ready() bool {
	return d.Control == Healthy && d.Store == Healthy && d.Carrier == Healthy && d.Usage != Unavailable
}

type Sources struct {
	Node           func() node.Snapshot
	Manager        func() node.ManagerSnapshot
	Sessions       func() int
	SessionRoutes  func() int
	ActiveStreams  func() uint32
	RouteCount     func() int
	Usage          func() usage.QueueStats
	ControlErr     func() error
	RouteErr       func() error
	UsageErr       func() error
	CarrierRunning func() bool
	Traffic        func() []usage.CounterRecord
	Health         func() edgetelemetry.HealthSnapshot
	Lifecycle      func() []edgetelemetry.Event
	TypedMetrics   func() []edgetelemetry.MetricSample
	TelemetryDrops func() uint64
	Now            func() time.Time
}

type Snapshot struct {
	At                    time.Time                     `json:"at"`
	Node                  node.Snapshot                 `json:"node"`
	Control               Status                        `json:"control"`
	Routes                Status                        `json:"routes"`
	Usage                 Status                        `json:"usage"`
	Carrier               Status                        `json:"carrier"`
	Connectors            int                           `json:"connectors"`
	ActiveStreams         uint32                        `json:"active_streams"`
	AttachedRoutes        int                           `json:"attached_routes"`
	RouteDrift            bool                          `json:"route_drift"`
	UsagePendingReports   int                           `json:"usage_pending_reports"`
	UsagePendingBytes     int                           `json:"usage_pending_bytes"`
	UsageOldestAgeSeconds int64                         `json:"usage_oldest_age_seconds"`
	Capacity              uint32                        `json:"connector_capacity"`
	FailureCodes          []string                      `json:"failure_codes,omitempty"`
	TrafficIngressBytes   uint64                        `json:"traffic_ingress_bytes"`
	TrafficEgressBytes    uint64                        `json:"traffic_egress_bytes"`
	Health                *edgetelemetry.HealthSnapshot `json:"health,omitempty"`
	LifecycleEvents       []edgetelemetry.Event         `json:"lifecycle_events,omitempty"`
	TelemetryDrops        uint64                        `json:"telemetry_drops"`
	TypedMetrics          []edgetelemetry.MetricSample  `json:"-"`
}

func NewHandler(s Sources) (http.Handler, error) {
	if s.Node == nil || s.Manager == nil || s.Sessions == nil || s.SessionRoutes == nil || s.ActiveStreams == nil || s.RouteCount == nil || s.Usage == nil || s.ControlErr == nil || s.RouteErr == nil || s.UsageErr == nil || s.CarrierRunning == nil || s.Traffic == nil {
		return nil, fmt.Errorf("observability sources are incomplete")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { writeSnapshot(w, snapshot(s), false) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { writeSnapshot(w, snapshot(s), true) })
	mux.HandleFunc("GET /diagnostics", func(w http.ResponseWriter, _ *http.Request) { writeSnapshot(w, snapshot(s), false) })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) { writeMetrics(w, snapshot(s)) })
	return mux, nil
}

func snapshot(s Sources) Snapshot {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	manager, pending := s.Manager(), s.Usage()
	routeErr := s.RouteErr()
	result := Snapshot{At: now, Node: s.Node(), Control: statusFor(s.ControlErr()), Routes: statusFor(routeErr), Usage: statusFor(s.UsageErr()), Carrier: runningStatus(s.CarrierRunning()), Connectors: s.Sessions(), ActiveStreams: s.ActiveStreams(), AttachedRoutes: s.RouteCount(), UsagePendingReports: pending.Reports, UsagePendingBytes: pending.Bytes, Capacity: manager.Capacity}
	if s.Health != nil {
		health := s.Health()
		result.Health = &health
	}
	if s.Lifecycle != nil {
		result.LifecycleEvents = s.Lifecycle()
	}
	if s.TypedMetrics != nil {
		result.TypedMetrics = s.TypedMetrics()
	}
	if s.TelemetryDrops != nil {
		result.TelemetryDrops = s.TelemetryDrops()
	}
	// Desired routes may legitimately outnumber active session routes while an
	// admitted connector is offline. Active routes must, however, always remain
	// a subset of the authoritative registry. A larger active set proves that a
	// stale or unauthorized route survived reconciliation.
	result.RouteDrift = s.SessionRoutes() > result.AttachedRoutes
	if result.RouteDrift {
		result.Routes = Degraded
	}
	if pending.Reports >= pending.MaxReports || pending.Bytes >= pending.MaxBytes {
		result.Usage = Unavailable
	}
	for _, record := range s.Traffic() {
		switch record.Key.Direction {
		case "ingress":
			result.TrafficIngressBytes += record.Bytes
		case "egress":
			result.TrafficEgressBytes += record.Bytes
		}
	}
	if !pending.OldestAt.IsZero() && now.After(pending.OldestAt) {
		result.UsageOldestAgeSeconds = int64(now.Sub(pending.OldestAt) / time.Second)
	}
	for name, status := range map[string]Status{"control_unavailable": result.Control, "usage_delivery_failed": result.Usage, "carrier_unavailable": result.Carrier} {
		if status != Healthy {
			result.FailureCodes = append(result.FailureCodes, name)
		}
	}
	if routeErr != nil {
		result.FailureCodes = append(result.FailureCodes, "route_reconciliation_failed")
	}
	if result.RouteDrift {
		result.FailureCodes = append(result.FailureCodes, "route_drift")
	}
	sort.Strings(result.FailureCodes)
	return result
}

func statusFor(err error) Status {
	if err != nil {
		return Degraded
	}
	return Healthy
}
func runningStatus(running bool) Status {
	if !running {
		return Unavailable
	}
	return Healthy
}

func (s Snapshot) ready() bool {
	return s.Node.Ready && s.Control == Healthy && s.Routes == Healthy && s.Usage != Unavailable && s.Carrier == Healthy
}

func writeSnapshot(w http.ResponseWriter, value Snapshot, readiness bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if readiness && !value.ready() || !readiness && !value.Node.Live {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(value)
}

func writeMetrics(w http.ResponseWriter, s Snapshot) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	lines := []string{
		"paperboat_tunnel_live " + booleanMetric(s.Node.Live),
		"paperboat_tunnel_ready " + booleanMetric(s.ready()),
		"paperboat_tunnel_connectors " + strconv.Itoa(s.Connectors),
		"paperboat_tunnel_active_streams " + strconv.FormatUint(uint64(s.ActiveStreams), 10),
		"paperboat_tunnel_attached_routes " + strconv.Itoa(s.AttachedRoutes),
		"paperboat_tunnel_connector_capacity " + strconv.FormatUint(uint64(s.Capacity), 10),
		"paperboat_tunnel_usage_pending_reports " + strconv.Itoa(s.UsagePendingReports),
		"paperboat_tunnel_usage_pending_bytes " + strconv.Itoa(s.UsagePendingBytes),
		"paperboat_tunnel_usage_oldest_age_seconds " + strconv.FormatInt(s.UsageOldestAgeSeconds, 10),
		"paperboat_tunnel_traffic_ingress_bytes_total " + strconv.FormatUint(s.TrafficIngressBytes, 10),
		"paperboat_tunnel_traffic_egress_bytes_total " + strconv.FormatUint(s.TrafficEgressBytes, 10),
	}
	for _, dependency := range []struct {
		name   string
		status Status
	}{{"control", s.Control}, {"routes", s.Routes}, {"usage", s.Usage}, {"carrier", s.Carrier}} {
		lines = append(lines, `paperboat_tunnel_dependency_healthy{dependency="`+dependency.name+`"} `+booleanMetric(dependency.status == Healthy))
	}
	for _, sample := range s.TypedMetrics {
		labels := metricLabels(sample.Labels)
		switch sample.Kind {
		case edgetelemetry.MetricCounter, edgetelemetry.MetricGauge:
			lines = append(lines, sample.Name+labels+" "+strconv.FormatUint(sample.Value, 10))
		case edgetelemetry.MetricHistogram:
			for _, bucket := range sample.Buckets {
				bucketLabels := append(append([]edgetelemetry.MetricLabel(nil), sample.Labels...), edgetelemetry.MetricLabel{Name: "le", Value: strconv.FormatFloat(bucket.UpperBound, 'g', -1, 64)})
				lines = append(lines, sample.Name+"_bucket"+metricLabels(bucketLabels)+" "+strconv.FormatUint(bucket.Count, 10))
			}
			bucketLabels := append(append([]edgetelemetry.MetricLabel(nil), sample.Labels...), edgetelemetry.MetricLabel{Name: "le", Value: "+Inf"})
			lines = append(lines, sample.Name+"_bucket"+metricLabels(bucketLabels)+" "+strconv.FormatUint(sample.Count, 10))
			lines = append(lines, sample.Name+"_sum"+labels+" "+strconv.FormatFloat(sample.Sum, 'g', -1, 64))
			lines = append(lines, sample.Name+"_count"+labels+" "+strconv.FormatUint(sample.Count, 10))
		}
	}
	lines = append(lines, "paperboat_edge_telemetry_dropped_total "+strconv.FormatUint(s.TelemetryDrops, 10))
	_, _ = w.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

func metricLabels(labels []edgetelemetry.MetricLabel) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, len(labels))
	for index, label := range labels {
		parts[index] = label.Name + `="` + label.Value + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func booleanMetric(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
