package route

import (
	"errors"
	"sync"
	"time"

	edgetelemetry "github.com/pinksaucepasta/paperboat-tunnel/internal/telemetry"

	"github.com/google/uuid"
)

type LifecycleType string

const (
	LifecycleStageAccepted    LifecycleType = "stage_accepted"
	LifecycleStageRejected    LifecycleType = "stage_rejected"
	LifecycleGenerationReady  LifecycleType = "generation_ready"
	LifecycleActivated        LifecycleType = "activated"
	LifecycleDrainStarted     LifecycleType = "drain_started"
	LifecycleDrainCompleted   LifecycleType = "drain_completed"
	LifecycleDrainForced      LifecycleType = "drain_forced"
	LifecycleStaleRejected    LifecycleType = "stale_generation_rejected"
	LifecycleStreamAcquired   LifecycleType = "stream_acquired"
	LifecycleStreamOverloaded LifecycleType = "stream_overloaded"
	LifecycleStreamReleased   LifecycleType = "stream_released"
)

type RouteTelemetryRecord struct {
	At                 time.Time
	Type               LifecycleType
	Generation         uint64
	PreviousGeneration uint64
	RouteCount         int
	ActiveStreams      int
	MaximumStreams     int
	CorrelationID      string
	IDs                edgetelemetry.SafeIDs
	Generations        edgetelemetry.Generations
}

type RouteTelemetrySink interface {
	RecordRouteTelemetry(RouteTelemetryRecord) error
}

type RouteTelemetrySinkFunc func(RouteTelemetryRecord) error

func (f RouteTelemetrySinkFunc) RecordRouteTelemetry(record RouteTelemetryRecord) error {
	if f == nil {
		return nil
	}
	return f(record)
}

// CoreTelemetrySink projects route lifecycle records into the dependency-safe
// health, fixed-cardinality metrics, and bounded event primitives.
type CoreTelemetrySink struct {
	mu         sync.Mutex
	health     *edgetelemetry.HealthTracker
	metrics    *edgetelemetry.Metrics
	events     *edgetelemetry.EventLog
	overloaded bool
	active     uint64
}

func NewCoreTelemetrySink(health *edgetelemetry.HealthTracker, metrics *edgetelemetry.Metrics, events *edgetelemetry.EventLog) (*CoreTelemetrySink, error) {
	if health == nil || metrics == nil || events == nil {
		return nil, ErrInvalid
	}
	return &CoreTelemetrySink{health: health, metrics: metrics, events: events}, nil
}

func (s *CoreTelemetrySink) RecordRouteTelemetry(record RouteTelemetryRecord) error {
	if s == nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	eventInput, err := routeEventInput(record)
	if err != nil {
		return err
	}
	_, _, eventErr := s.events.TryRecord(eventInput)
	var healthErr error
	switch record.Type {
	case LifecycleStageAccepted:
		healthErr = s.updateHealth(edgetelemetry.DimensionConfig, edgetelemetry.StatusDegraded, "generation_staged", "A route generation is staged for readiness checks.", "Complete readiness checks and activate the generation.", record, edgetelemetry.RetryWaitForChange)
	case LifecycleStageRejected:
		healthErr = s.updateHealth(edgetelemetry.DimensionConfig, edgetelemetry.StatusDegraded, "generation_rejected", "A route generation was rejected.", "Correct the route generation and stage it again.", record, edgetelemetry.RetryWaitForChange)
	case LifecycleGenerationReady:
		healthErr = s.updateHealth(edgetelemetry.DimensionConfig, edgetelemetry.StatusReady, "generation_ready", "The staged route generation passed readiness checks.", "Activate the ready route generation.", record, edgetelemetry.RetryNone)
	case LifecycleActivated:
		s.active = record.Generation
		s.overloaded = false
		configErr := s.updateHealth(edgetelemetry.DimensionConfig, edgetelemetry.StatusReady, "generation_active", "The active route configuration is current.", "No action is required.", record, edgetelemetry.RetryNone)
		routeErr := s.updateHealth(edgetelemetry.DimensionRoute, edgetelemetry.StatusReady, "ready", "Route admission is ready.", "No action is required.", record, edgetelemetry.RetryNone)
		healthErr = errors.Join(configErr, routeErr)
	case LifecycleDrainForced:
		healthErr = s.updateHealth(edgetelemetry.DimensionRoute, edgetelemetry.StatusDegraded, "drain_forced", "An old route generation required forced stream cancellation.", "Inspect slow streams before the next route activation.", record, edgetelemetry.RetryNone)
	case LifecycleStreamOverloaded:
		if record.Generation != s.active {
			break
		}
		s.overloaded = true
		healthErr = s.updateHealth(edgetelemetry.DimensionRoute, edgetelemetry.StatusDegraded, "stream_capacity_reached", "Route stream capacity is exhausted.", "Wait for active streams to finish or increase the configured capacity.", record, edgetelemetry.RetryWaitForChange)
	case LifecycleStreamReleased:
		if record.Generation == s.active && s.overloaded && record.ActiveStreams < record.MaximumStreams {
			s.overloaded = false
			healthErr = s.updateHealth(edgetelemetry.DimensionRoute, edgetelemetry.StatusReady, "ready", "Route stream capacity is available.", "No action is required.", record, edgetelemetry.RetryNone)
		}
	}
	return errors.Join(eventErr, healthErr)
}

func (s *CoreTelemetrySink) updateHealth(dimension edgetelemetry.Dimension, status edgetelemetry.HealthStatus, code, summary, repair string, record RouteTelemetryRecord, retry edgetelemetry.RetryDecision) error {
	before := s.health.Snapshot().Dimensions.Get(dimension)
	if err := s.health.Update(edgetelemetry.HealthUpdate{
		Dimension: dimension, Status: status, Code: code, Summary: summary, RepairAction: repair,
		CorrelationID: record.CorrelationID, Retry: retry,
	}); err != nil {
		return err
	}
	after := s.health.Snapshot().Dimensions.Get(dimension)
	var metricErr error
	if before.Status != after.Status {
		metricErr = s.metrics.AddCounter(edgetelemetry.MetricHealthTransitions, edgetelemetry.MetricLabels{
			"dimension": string(dimension), "from": string(before.Status), "to": string(after.Status),
		}, 1)
	}
	oldGaugeErr := s.metrics.SetGauge(edgetelemetry.MetricHealthDimension, edgetelemetry.MetricLabels{
		"dimension": string(dimension), "status": string(before.Status),
	}, 0)
	newGaugeErr := s.metrics.SetGauge(edgetelemetry.MetricHealthDimension, edgetelemetry.MetricLabels{
		"dimension": string(dimension), "status": string(after.Status),
	}, 1)
	return errors.Join(metricErr, oldGaugeErr, newGaugeErr)
}

// EventInput projects only the finite route lifecycle catalog. Operational IDs
// remain available to the local route owner; exporters select their own fields.
func (record RouteTelemetryRecord) EventInput() (edgetelemetry.EventInput, error) {
	return routeEventInput(record)
}

func routeEventInput(record RouteTelemetryRecord) (edgetelemetry.EventInput, error) {
	severity, outcome, message, retry := edgetelemetry.SeverityInfo, edgetelemetry.OutcomeSuccess, "Route lifecycle transition completed.", edgetelemetry.RetryNone
	switch record.Type {
	case LifecycleStageAccepted:
		message = "Route generation was staged."
	case LifecycleStageRejected:
		severity, outcome, message, retry = edgetelemetry.SeverityWarn, edgetelemetry.OutcomeRejected, "Route generation was rejected.", edgetelemetry.RetryWaitForChange
	case LifecycleGenerationReady:
		message = "Route generation passed readiness checks."
	case LifecycleActivated:
		outcome, message = edgetelemetry.OutcomeStateChange, "Route generation was activated."
	case LifecycleDrainStarted:
		outcome, message = edgetelemetry.OutcomeStateChange, "Old route generation started draining."
	case LifecycleDrainCompleted:
		message = "Old route generation completed draining."
	case LifecycleDrainForced:
		severity, outcome, message = edgetelemetry.SeverityWarn, edgetelemetry.OutcomeFailed, "Old route generation required forced cancellation."
	case LifecycleStaleRejected:
		severity, outcome, message, retry = edgetelemetry.SeverityWarn, edgetelemetry.OutcomeRejected, "Stale route generation was rejected.", edgetelemetry.RetryWaitForChange
	case LifecycleStreamAcquired:
		severity, message = edgetelemetry.SeverityDebug, "Route stream was acquired."
	case LifecycleStreamOverloaded:
		severity, outcome, message, retry = edgetelemetry.SeverityWarn, edgetelemetry.OutcomeRejected, "Route stream capacity was exhausted.", edgetelemetry.RetryWaitForChange
	case LifecycleStreamReleased:
		severity, message = edgetelemetry.SeverityDebug, "Route stream was released."
	default:
		return edgetelemetry.EventInput{}, ErrInvalid
	}
	return edgetelemetry.EventInput{
		At: record.At, Severity: severity, Component: edgetelemetry.DimensionRoute,
		Name: string(record.Type), Code: string(record.Type), Outcome: outcome, Message: message,
		CorrelationID: record.CorrelationID, IDs: record.IDs, Generations: record.Generations, Retry: retry,
	}, nil
}

func routeTelemetryIdentity(generation uint64, rules []RouteRule) (string, edgetelemetry.SafeIDs, edgetelemetry.Generations) {
	correlationID := "correlation_" + uuid.NewString()
	ids := edgetelemetry.SafeIDs{}
	generations := edgetelemetry.Generations{Route: generation}
	if len(rules) == 0 {
		return correlationID, ids, generations
	}
	rule := rules[0]
	ids.RouteID = allowedTelemetryID(firstNonempty(rule.RouteID, rule.ID))
	ids.TunnelID = allowedTelemetryID(rule.TunnelID)
	ids.ConnectorID = allowedTelemetryID(rule.ConnectorID)
	ids.SessionID = allowedTelemetryID(rule.ConnectorSessionID)
	generations.Config = firstNonzero(rule.ConfigGeneration, generation)
	generations.Route = firstNonzero(rule.RouteGeneration, generation)
	generations.Assignment = rule.AssignmentGeneration
	generations.Connector = rule.ConnectorProcessGeneration
	generations.Session = rule.SessionGeneration
	return correlationID, ids, generations
}

func allowedTelemetryID(value string) string {
	if edgetelemetry.SafeOpaqueID(value) {
		return value
	}
	return ""
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonzero(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
