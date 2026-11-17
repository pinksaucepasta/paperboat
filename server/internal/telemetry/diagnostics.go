package telemetry

import (
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/shared/health"
	"github.com/pinksaucepasta/paperboat/shared/telemetry/safe"
	"math"
	"sort"
	"sync"
	"time"
)

// DiagnosticsSchemaV1 is the private, local operator snapshot. It is kept
// separate from the public resource schema because this document contains
// process-local telemetry rather than account or resource state.
const DiagnosticsSchemaV1 = "paperboat.server-diagnostics/v1"

const (
	// These limits are deliberately smaller than the HTTP response ceiling. A
	// future field cannot turn a local diagnostics request into an unbounded
	// memory or response allocation.
	MaximumDiagnosticsEvents  = 256
	MaximumDiagnosticsMetrics = 2048
	MaximumDiagnosticsBytes   = 1 << 20
)

// DiagnosticsConfig supplies the bounded telemetry sources used by a local
// diagnostics snapshot. EventLog may be attached later with SetEventLog when
// the application creates its per-run event worker.
type DiagnosticsConfig struct {
	Metrics *Metrics
	Health  *health.HealthTracker
	Events  *EventLog
	Now     func() time.Time

	MaximumEvents  int
	MaximumMetrics int
	MaximumBytes   int
}

// Diagnostics is a small, concurrency-safe view over the live server
// telemetry sinks. It owns no sink and never closes one supplied by callers.
type Diagnostics struct {
	mu      sync.RWMutex
	metrics *Metrics
	health  *health.HealthTracker
	events  *EventLog
	now     func() time.Time

	maximumEvents  int
	maximumMetrics int
	maximumBytes   int
}

func NewDiagnostics(config DiagnosticsConfig) (*Diagnostics, error) {
	maximumEvents := config.MaximumEvents
	if maximumEvents == 0 {
		maximumEvents = MaximumDiagnosticsEvents
	}
	maximumMetrics := config.MaximumMetrics
	if maximumMetrics == 0 {
		maximumMetrics = MaximumDiagnosticsMetrics
	}
	maximumBytes := config.MaximumBytes
	if maximumBytes == 0 {
		maximumBytes = MaximumDiagnosticsBytes
	}
	if maximumEvents < 0 || maximumEvents > maximumEventLogSize || maximumMetrics < 0 || maximumMetrics > defaultMaximumMetricSeries*2 || maximumBytes < 0 || maximumBytes > 16<<20 {
		return nil, safe.NewError(safe.ErrorInvalidCapacity, "construct control-plane diagnostics")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	if safe.NormalizeTime(now()).IsZero() {
		return nil, safe.NewError(safe.ErrorInvalidTime, "construct control-plane diagnostics")
	}
	return &Diagnostics{
		metrics:        config.Metrics,
		health:         config.Health,
		events:         config.Events,
		now:            now,
		maximumEvents:  maximumEvents,
		maximumMetrics: maximumMetrics,
		maximumBytes:   maximumBytes,
	}, nil
}

// SetEventLog attaches the current application-run event log. It is safe to
// call while a diagnostics request is taking a snapshot.
func (d *Diagnostics) SetEventLog(events *EventLog) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.events = events
	d.mu.Unlock()
}

// Snapshot returns a bounded, detached view of the current telemetry. The
// returned value contains no pointers into mutable telemetry state.
func (d *Diagnostics) Snapshot() DiagnosticsSnapshot {
	if d == nil {
		return DiagnosticsSnapshot{}
	}
	d.mu.RLock()
	metrics, healthTracker, events, now := d.metrics, d.health, d.events, d.now
	maximumEvents, maximumMetrics := d.maximumEvents, d.maximumMetrics
	d.mu.RUnlock()

	if now == nil {
		now = time.Now
	}
	snapshot := DiagnosticsSnapshot{
		Schema:     DiagnosticsSchemaV1,
		CapturedAt: safe.NormalizeTime(now()),
		Metrics:    []MetricSample{},
		Events:     []Event{},
	}
	if healthTracker != nil {
		snapshot.Health = healthTracker.Snapshot()
		snapshot.Retry = snapshot.Health.Overall.Retry
		snapshot.NextRetryAt = safe.CloneTime(snapshot.Health.Overall.NextRetryAt)
	}
	if metrics != nil {
		snapshot.Metrics = metrics.Snapshot()
		if maximumMetrics >= 0 && len(snapshot.Metrics) > maximumMetrics {
			snapshot.Metrics = snapshot.Metrics[:maximumMetrics]
		}
	}
	if events != nil {
		snapshot.Events = events.Snapshot()
		if maximumEvents >= 0 && len(snapshot.Events) > maximumEvents {
			snapshot.Events = snapshot.Events[len(snapshot.Events)-maximumEvents:]
		}
		snapshot.DroppedEvents = events.DroppedEvents()
	}
	return snapshot
}

// SnapshotWithIdentity adds identities assigned by the trusted HTTP
// middleware. It is useful for correlating a support snapshot with the
// request that retrieved it; no raw request fields are copied.
func (d *Diagnostics) SnapshotWithIdentity(requestID, correlationID string) DiagnosticsSnapshot {
	snapshot := d.Snapshot()
	snapshot.RequestID = requestID
	snapshot.CorrelationID = correlationID
	return snapshot
}

// JSONWithIdentity serializes the current view using the configured response
// limit. The snapshot is detached before serialization, so callers never hold
// a telemetry lock while encoding JSON.
func (d *Diagnostics) JSONWithIdentity(requestID, correlationID string) ([]byte, error) {
	if d == nil {
		return nil, safe.NewError(safe.ErrorInvalidObservation, "serialize control-plane diagnostics")
	}
	snapshot := d.SnapshotWithIdentity(requestID, correlationID)
	body, err := snapshot.JSON()
	if err != nil {
		return nil, err
	}
	d.mu.RLock()
	maximumBytes := d.maximumBytes
	d.mu.RUnlock()
	if maximumBytes > 0 && len(body) > maximumBytes {
		return nil, safe.NewError(safe.ErrorMetricCardinality, "serialize control-plane diagnostics")
	}
	return body, nil
}

// JSON serializes a detached snapshot and enforces the response bound even if
// callers construct a snapshot directly rather than using Diagnostics.
func (s DiagnosticsSnapshot) JSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(s)
	if err != nil {
		return nil, safe.NewError(safe.ErrorInvalidObservation, "serialize control-plane diagnostics")
	}
	if len(body) > MaximumDiagnosticsBytes {
		return nil, safe.NewError(safe.ErrorMetricCardinality, "serialize control-plane diagnostics")
	}
	return body, nil
}

// SupportSnapshot is a descriptive alias for callers that expose this
// document as a support-bundle preview.
type SupportSnapshot = DiagnosticsSnapshot

// DiagnosticsSnapshot is intentionally limited to the typed sinks. Metric
// labels and event fields are already allowlisted/redacted at construction.
type DiagnosticsSnapshot struct {
	Schema        string                `json:"schema"`
	CapturedAt    time.Time             `json:"captured_at"`
	RequestID     string                `json:"request_id,omitempty"`
	CorrelationID string                `json:"correlation_id,omitempty"`
	Health        health.HealthSnapshot `json:"health"`
	Retry         health.RetryDecision  `json:"retry"`
	NextRetryAt   *time.Time            `json:"next_retry_at,omitempty"`
	Metrics       []MetricSample        `json:"metrics"`
	Events        []Event               `json:"events"`
	DroppedEvents uint64                `json:"dropped_events"`
}

func (s DiagnosticsSnapshot) Validate() error {
	if s.Schema != DiagnosticsSchemaV1 || safe.NormalizeTime(s.CapturedAt).IsZero() || !validOptionalLogIdentity(s.RequestID, "request_", "req_") || (s.CorrelationID != "" && !safe.CorrelationID(s.CorrelationID)) || !health.ValidRetry(s.Retry, safe.TimeValue(s.NextRetryAt)) || len(s.Metrics) > MaximumDiagnosticsMetrics || len(s.Events) > MaximumDiagnosticsEvents {
		return safe.NewError(safe.ErrorInvalidObservation, "validate control-plane diagnostics")
	}
	if err := s.Health.Validate(); err != nil {
		return err
	}
	for _, sample := range s.Metrics {
		if err := validateMetricSample(sample); err != nil {
			return err
		}
	}
	for _, event := range s.Events {
		if err := event.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateMetricSample(sample MetricSample) error {
	descriptor, ok := descriptorByName(sample.Name)
	if !ok || sample.Kind != descriptor.Kind || len(sample.Labels) != len(descriptor.Labels) || math.IsNaN(sample.Sum) || math.IsInf(sample.Sum, 0) || sample.Sum < 0 {
		return safe.NewError(safe.ErrorInvalidObservation, "validate control-plane metric")
	}
	for index, label := range sample.Labels {
		if label.Name != descriptor.Labels[index].Name || !containsString(descriptor.Labels[index].AllowedValues, label.Value) {
			return safe.NewError(safe.ErrorInvalidObservation, "validate control-plane metric")
		}
	}
	if descriptor.Kind == MetricHistogram {
		if descriptor.Histogram == nil || len(sample.Buckets) != len(descriptor.Histogram.Buckets) {
			return safe.NewError(safe.ErrorInvalidObservation, "validate control-plane metric")
		}
		last := 0.0
		for index, bucket := range sample.Buckets {
			if bucket.UpperBound != descriptor.Histogram.Buckets[index] || bucket.UpperBound < last {
				return safe.NewError(safe.ErrorInvalidObservation, "validate control-plane metric")
			}
			last = bucket.UpperBound
		}
	} else if len(sample.Buckets) != 0 {
		return safe.NewError(safe.ErrorInvalidObservation, "validate control-plane metric")
	}
	return nil
}

func descriptorByName(name string) (MetricDescriptor, bool) {
	for _, descriptor := range fixedMetricDescriptors {
		if descriptor.Name == name {
			return descriptor, true
		}
	}
	return MetricDescriptor{}, false
}

// Sort keeps deterministic ordering explicit for callers that provide a copied
// snapshot with slices assembled from multiple sources.
func (s *DiagnosticsSnapshot) Sort() {
	if s == nil {
		return
	}
	sort.SliceStable(s.Metrics, func(i, j int) bool {
		if s.Metrics[i].Name != s.Metrics[j].Name {
			return s.Metrics[i].Name < s.Metrics[j].Name
		}
		return metricLabelsKey(s.Metrics[i].Labels) < metricLabelsKey(s.Metrics[j].Labels)
	})
}

func metricLabelsKey(labels []MetricLabel) string {
	key := ""
	for _, label := range labels {
		key += label.Name + "\x1f" + label.Value + "\x1e"
	}
	return key
}
