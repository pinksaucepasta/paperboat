package telemetry

import (
	"bytes"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/shared/health"
	sharedtelemetry "github.com/pinksaucepasta/paperboat/shared/telemetry"
	"github.com/pinksaucepasta/paperboat/shared/telemetry/safe"
	"sync"
	"sync/atomic"
	"time"
)

// LifecycleSchemaV1 identifies the version used by lifecycle records. Lifecycle records use the same versioned resource envelope as the rest of
// the preview/tunnel API. They are intentionally richer than Event, which is
// the small edge-compatible transport event retained for existing producers.
const LifecycleSchemaV1 = sharedtelemetry.ContractSchemaV1

const (
	maximumLifecycleEventType = 80
	maximumLifecycleCode      = 64
	maximumLifecycleLogSize   = 4096
)

// LifecycleEventInput is the server-owned lifecycle boundary. Request,
// correlation, resource, and operation identities are required. All other
// values are metadata and are redacted or rejected before the record is
// returned to the caller.
type LifecycleEventInput struct {
	ID     string
	Cursor string

	At         time.Time
	OccurredAt time.Time

	Severity  sharedtelemetry.EventSeverity
	Level     sharedtelemetry.EventSeverity
	Component health.Dimension

	EventType string
	Name      string
	Code      string
	Outcome   sharedtelemetry.EventOutcome
	Message   string

	RequestID     string
	CorrelationID string

	ResourceKind string
	ResourceID   string
	Resource     ResourceIdentity

	OperationID   string
	OperationKind string
	Operation     OperationIdentity

	ActorType string
	ActorID   string

	Generations  sharedtelemetry.Generations
	Retry        health.RetryDecision
	NextRetryAt  time.Time
	Metadata     map[string]any
	SafeMetadata map[string]any
}

// ResourceIdentity is a typed, opaque resource reference. It never carries a
// hostname, URL, route expression, or user-provided name.
type ResourceIdentity struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// OperationIdentity is a typed reference to the durable operation that caused
// a lifecycle transition.
type OperationIdentity struct {
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id"`
}

// LifecycleEvent is the immutable structured record retained in the
// lifecycle stream. Metadata is a deep copy made by the constructor.
type LifecycleEvent struct {
	Schema        string                          `json:"schema"`
	Kind          string                          `json:"kind"`
	ID            string                          `json:"id,omitempty"`
	Cursor        string                          `json:"cursor,omitempty"`
	EventType     string                          `json:"event_type"`
	OccurredAt    time.Time                       `json:"occurred_at"`
	Severity      sharedtelemetry.EventSeverity   `json:"severity"`
	Component     health.Dimension                `json:"component"`
	Code          string                          `json:"code"`
	Outcome       sharedtelemetry.EventOutcome    `json:"outcome"`
	Message       string                          `json:"message"`
	RequestID     string                          `json:"request_id"`
	CorrelationID string                          `json:"correlation_id"`
	Resource      ResourceIdentity                `json:"resource"`
	Operation     OperationIdentity               `json:"operation"`
	Actor         *sharedtelemetry.CanonicalActor `json:"actor,omitempty"`
	Generations   sharedtelemetry.Generations     `json:"generations,omitempty"`
	Retry         health.RetryDecision            `json:"retry"`
	NextRetryAt   *time.Time                      `json:"next_retry_at,omitempty"`
	Metadata      map[string]any                  `json:"metadata"`
}

// LifecycleEventRecord is a descriptive alias used by persistence adapters.
type LifecycleEventRecord = LifecycleEvent

// NewLifecycleEvent constructs and validates one lifecycle event. It resolves
// the old Name/At aliases for callers that share input structs with Event.
func NewLifecycleEvent(input LifecycleEventInput) (LifecycleEvent, error) {
	normalized, err := normalizeLifecycleInput(input)
	if err != nil {
		return LifecycleEvent{}, err
	}
	event := LifecycleEvent{
		Schema:        LifecycleSchemaV1,
		Kind:          sharedtelemetry.KindLifecycleEvent,
		ID:            normalized.id,
		Cursor:        normalized.cursor,
		EventType:     normalized.eventType,
		OccurredAt:    normalized.at,
		Severity:      normalized.severity,
		Component:     normalized.component,
		Code:          normalized.code,
		Outcome:       normalized.outcome,
		Message:       normalized.message,
		RequestID:     normalized.requestID,
		CorrelationID: normalized.correlationID,
		Resource:      normalized.resource,
		Operation:     normalized.operation,
		Generations:   normalized.generations,
		Retry:         normalized.retry,
		Metadata:      normalized.metadata,
	}
	if normalized.actor != nil {
		actor := *normalized.actor
		event.Actor = &actor
	}
	if !normalized.nextRetryAt.IsZero() {
		next := normalized.nextRetryAt
		event.NextRetryAt = &next
	}
	return event, event.Validate()
}

func (e LifecycleEvent) Validate() error {
	if e.Schema != LifecycleSchemaV1 || e.Kind != sharedtelemetry.KindLifecycleEvent || !validOptionalOpaqueID(e.ID) || !validOptionalOpaqueID(e.Cursor) || !sharedtelemetry.ValidEventType(e.EventType) || len(e.EventType) > maximumLifecycleEventType || e.OccurredAt.IsZero() || !sharedtelemetry.ValidEventSeverity(e.Severity) || !health.ValidDimension(e.Component) || !safe.StableCode(e.Code) || len(e.Code) > maximumLifecycleCode || !sharedtelemetry.ValidEventOutcome(e.Outcome) || !safe.StoredText(e.Message, safe.MaximumMessageBytes) || !sharedtelemetry.ValidOptionalLogIdentity(e.RequestID, "request_", "req_") || !safe.CorrelationID(e.CorrelationID) || !sharedtelemetry.ValidEventResourceKind(e.Resource.Kind) || !sharedtelemetry.ValidOpaqueID(e.Resource.ID) || !sharedtelemetry.ValidOptionalLogIdentity(e.Operation.ID, "operation_", "op_") || e.Operation.ID == "" || (e.Operation.Kind != "" && !safe.StableCode(e.Operation.Kind)) || !health.ValidRetry(e.Retry, safe.TimeValue(e.NextRetryAt)) {
		return safe.NewError(safe.ErrorInvalidEvent, "validate lifecycle event")
	}
	if e.RequestID == "" {
		return safe.NewError(safe.ErrorIdentityRequired, "validate lifecycle event")
	}
	if e.Actor != nil && (!sharedtelemetry.ValidActorType(e.Actor.Type) || !sharedtelemetry.ValidOpaqueID(e.Actor.ID)) {
		return safe.NewError(safe.ErrorInvalidID, "validate lifecycle event")
	}
	if !metadataIsSafe(e.Metadata) {
		return safe.NewError(safe.ErrorUnsafeField, "validate lifecycle event")
	}
	return nil
}

func (e LifecycleEvent) JSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// LogRecord is the structured diagnostic counterpart to LifecycleEvent.
// It carries the same required identities so an operator can correlate logs,
// events, and API requests without searching unstructured text.
type LogRecord struct {
	Schema        string                        `json:"schema"`
	Kind          string                        `json:"kind"`
	ID            string                        `json:"id,omitempty"`
	Cursor        string                        `json:"cursor,omitempty"`
	OccurredAt    time.Time                     `json:"occurred_at"`
	Level         sharedtelemetry.EventSeverity `json:"level"`
	Component     health.Dimension              `json:"component"`
	Code          string                        `json:"code"`
	Message       string                        `json:"message"`
	RequestID     string                        `json:"request_id"`
	CorrelationID string                        `json:"correlation_id"`
	Resource      ResourceIdentity              `json:"resource"`
	Operation     OperationIdentity             `json:"operation"`
	Generations   sharedtelemetry.Generations   `json:"generations,omitempty"`
	Retry         health.RetryDecision          `json:"retry"`
	NextRetryAt   *time.Time                    `json:"next_retry_at,omitempty"`
	Metadata      map[string]any                `json:"metadata"`
}

type LifecycleLogRecord = LogRecord
type StructuredLogRecord = LogRecord

type LogRecordInput struct {
	ID     string
	Cursor string

	At         time.Time
	OccurredAt time.Time
	Level      sharedtelemetry.EventSeverity
	Severity   sharedtelemetry.EventSeverity
	Component  health.Dimension
	Code       string
	Message    string

	RequestID     string
	CorrelationID string
	ResourceKind  string
	ResourceID    string
	Resource      ResourceIdentity
	OperationID   string
	OperationKind string
	Operation     OperationIdentity

	Generations sharedtelemetry.Generations
	Retry       health.RetryDecision
	NextRetryAt time.Time
	Metadata    map[string]any
}

type LifecycleLogInput = LogRecordInput

func NewLogRecord(input LogRecordInput) (LogRecord, error) {
	normalized, err := normalizeLogInput(input)
	if err != nil {
		return LogRecord{}, err
	}
	record := LogRecord{
		Schema:        LifecycleSchemaV1,
		Kind:          sharedtelemetry.KindLifecycleLog,
		ID:            normalized.id,
		Cursor:        normalized.cursor,
		OccurredAt:    normalized.at,
		Level:         normalized.level,
		Component:     normalized.component,
		Code:          normalized.code,
		Message:       normalized.message,
		RequestID:     normalized.requestID,
		CorrelationID: normalized.correlationID,
		Resource:      normalized.resource,
		Operation:     normalized.operation,
		Generations:   normalized.generations,
		Retry:         normalized.retry,
		Metadata:      normalized.metadata,
	}
	if !normalized.nextRetryAt.IsZero() {
		next := normalized.nextRetryAt
		record.NextRetryAt = &next
	}
	return record, record.Validate()
}

func (r LogRecord) Validate() error {
	if r.Schema != LifecycleSchemaV1 || r.Kind != sharedtelemetry.KindLifecycleLog || !validOptionalOpaqueID(r.ID) || !validOptionalOpaqueID(r.Cursor) || r.OccurredAt.IsZero() || !sharedtelemetry.ValidEventSeverity(r.Level) || !health.ValidDimension(r.Component) || !safe.StableCode(r.Code) || !safe.StoredText(r.Message, safe.MaximumMessageBytes) || !sharedtelemetry.ValidOptionalLogIdentity(r.RequestID, "request_", "req_") || !safe.CorrelationID(r.CorrelationID) || !sharedtelemetry.ValidEventResourceKind(r.Resource.Kind) || !sharedtelemetry.ValidOpaqueID(r.Resource.ID) || !sharedtelemetry.ValidOptionalLogIdentity(r.Operation.ID, "operation_", "op_") || r.Operation.ID == "" || (r.Operation.Kind != "" && !safe.StableCode(r.Operation.Kind)) || !health.ValidRetry(r.Retry, safe.TimeValue(r.NextRetryAt)) {
		return safe.NewError(safe.ErrorInvalidEvent, "validate lifecycle log")
	}
	if r.RequestID == "" {
		return safe.NewError(safe.ErrorIdentityRequired, "validate lifecycle log")
	}
	if !metadataIsSafe(r.Metadata) {
		return safe.NewError(safe.ErrorUnsafeField, "validate lifecycle log")
	}
	return nil
}

func (r LogRecord) JSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

type lifecycleNormalized struct {
	id, cursor, eventType, requestID, correlationID string
	at                                              time.Time
	severity                                        sharedtelemetry.EventSeverity
	component                                       health.Dimension
	code                                            string
	outcome                                         sharedtelemetry.EventOutcome
	message                                         string
	resource                                        ResourceIdentity
	operation                                       OperationIdentity
	actor                                           *sharedtelemetry.CanonicalActor
	generations                                     sharedtelemetry.Generations
	retry                                           health.RetryDecision
	nextRetryAt                                     time.Time
	metadata                                        map[string]any
}

type logNormalized struct {
	id, cursor, requestID, correlationID string
	at                                   time.Time
	level                                sharedtelemetry.EventSeverity
	component                            health.Dimension
	code, message                        string
	resource                             ResourceIdentity
	operation                            OperationIdentity
	generations                          sharedtelemetry.Generations
	retry                                health.RetryDecision
	nextRetryAt                          time.Time
	metadata                             map[string]any
}

func normalizeLifecycleInput(input LifecycleEventInput) (lifecycleNormalized, error) {
	at := input.OccurredAt
	if at.IsZero() {
		at = input.At
	}
	at = safe.NormalizeTime(at)
	if at.IsZero() {
		return lifecycleNormalized{}, safe.NewError(safe.ErrorInvalidTime, "construct lifecycle event")
	}
	severity := input.Severity
	if severity == "" {
		severity = input.Level
	}
	if !sharedtelemetry.ValidEventSeverity(severity) || !health.ValidDimension(input.Component) {
		return lifecycleNormalized{}, safe.NewError(safe.ErrorInvalidEvent, "construct lifecycle event")
	}
	eventType := input.EventType
	if eventType == "" {
		eventType = input.Name
	}
	if !sharedtelemetry.ValidEventType(eventType) || len(eventType) > maximumLifecycleEventType || !safe.StableCode(input.Code) || len(input.Code) > maximumLifecycleCode || !sharedtelemetry.ValidEventOutcome(input.Outcome) {
		return lifecycleNormalized{}, safe.NewError(safe.ErrorInvalidEvent, "construct lifecycle event")
	}
	requestID, correlationID, resource, operation, err := normalizeIdentities(input.RequestID, input.CorrelationID, input.ResourceKind, input.ResourceID, input.Resource, input.OperationID, input.OperationKind, input.Operation)
	if err != nil {
		return lifecycleNormalized{}, err
	}
	message, err := safe.BoundedString(input.Message, safe.MaximumMessageBytes, true)
	if err != nil {
		return lifecycleNormalized{}, err
	}
	metadata, err := selectMetadata(input.Metadata, input.SafeMetadata)
	if err != nil {
		return lifecycleNormalized{}, err
	}
	retry := input.Retry
	if retry == "" {
		retry = health.RetryNone
	}
	if !health.ValidRetry(retry, input.NextRetryAt) {
		return lifecycleNormalized{}, safe.NewError(safe.ErrorInvalidRetry, "construct lifecycle event")
	}
	var actor *sharedtelemetry.CanonicalActor
	if input.ActorType != "" || input.ActorID != "" {
		if !sharedtelemetry.ValidActorType(input.ActorType) || !sharedtelemetry.ValidOpaqueID(input.ActorID) {
			return lifecycleNormalized{}, safe.NewError(safe.ErrorInvalidID, "construct lifecycle event")
		}
		actor = &sharedtelemetry.CanonicalActor{Type: input.ActorType, ID: input.ActorID}
	}
	return lifecycleNormalized{id: input.ID, cursor: input.Cursor, eventType: eventType, at: at, severity: severity, component: input.Component, code: input.Code, outcome: input.Outcome, message: message, requestID: requestID, correlationID: correlationID, resource: resource, operation: operation, actor: actor, generations: input.Generations, retry: retry, nextRetryAt: safe.NormalizeTime(input.NextRetryAt), metadata: metadata}, nil
}

func normalizeLogInput(input LogRecordInput) (logNormalized, error) {
	at := input.OccurredAt
	if at.IsZero() {
		at = input.At
	}
	at = safe.NormalizeTime(at)
	if at.IsZero() {
		return logNormalized{}, safe.NewError(safe.ErrorInvalidTime, "construct lifecycle log")
	}
	level := input.Level
	if level == "" {
		level = input.Severity
	}
	if !sharedtelemetry.ValidEventSeverity(level) || !health.ValidDimension(input.Component) || !safe.StableCode(input.Code) {
		return logNormalized{}, safe.NewError(safe.ErrorInvalidEvent, "construct lifecycle log")
	}
	requestID, correlationID, resource, operation, err := normalizeIdentities(input.RequestID, input.CorrelationID, input.ResourceKind, input.ResourceID, input.Resource, input.OperationID, input.OperationKind, input.Operation)
	if err != nil {
		return logNormalized{}, err
	}
	message, err := safe.BoundedString(input.Message, safe.MaximumMessageBytes, true)
	if err != nil {
		return logNormalized{}, err
	}
	metadata, err := sharedtelemetry.SafeMetadata(input.Metadata)
	if err != nil {
		return logNormalized{}, err
	}
	retry := input.Retry
	if retry == "" {
		retry = health.RetryNone
	}
	if !health.ValidRetry(retry, input.NextRetryAt) {
		return logNormalized{}, safe.NewError(safe.ErrorInvalidRetry, "construct lifecycle log")
	}
	return logNormalized{id: input.ID, cursor: input.Cursor, at: at, level: level, component: input.Component, code: input.Code, message: message, requestID: requestID, correlationID: correlationID, resource: resource, operation: operation, generations: input.Generations, retry: retry, nextRetryAt: safe.NormalizeTime(input.NextRetryAt), metadata: metadata}, nil
}

func normalizeIdentities(requestID, correlationID, resourceKind, resourceID string, resource ResourceIdentity, operationID, operationKind string, operation OperationIdentity) (string, string, ResourceIdentity, OperationIdentity, error) {
	if resource.Kind != "" || resource.ID != "" {
		if resourceKind != "" && resourceKind != resource.Kind || resourceID != "" && resourceID != resource.ID {
			return "", "", ResourceIdentity{}, OperationIdentity{}, safe.NewError(safe.ErrorIdentityMismatch, "construct lifecycle telemetry")
		}
		if resourceKind == "" {
			resourceKind = resource.Kind
		}
		if resourceID == "" {
			resourceID = resource.ID
		}
	}
	if operation.Kind != "" || operation.ID != "" {
		if operationID != "" && operationID != operation.ID || operationKind != "" && operationKind != operation.Kind {
			return "", "", ResourceIdentity{}, OperationIdentity{}, safe.NewError(safe.ErrorIdentityMismatch, "construct lifecycle telemetry")
		}
		if operationID == "" {
			operationID = operation.ID
		}
		if operationKind == "" {
			operationKind = operation.Kind
		}
	}
	if !sharedtelemetry.ValidOptionalLogIdentity(requestID, "request_", "req_") || requestID == "" || !safe.CorrelationID(correlationID) || !sharedtelemetry.ValidEventResourceKind(resourceKind) || !sharedtelemetry.ValidOpaqueID(resourceID) || !sharedtelemetry.ValidOptionalLogIdentity(operationID, "operation_", "op_") || operationID == "" || (operationKind != "" && !safe.StableCode(operationKind)) {
		return "", "", ResourceIdentity{}, OperationIdentity{}, safe.NewError(safe.ErrorIdentityRequired, "construct lifecycle telemetry")
	}
	return requestID, correlationID, ResourceIdentity{Kind: resourceKind, ID: resourceID}, OperationIdentity{Kind: operationKind, ID: operationID}, nil
}

func selectMetadata(metadata, safeValues map[string]any) (map[string]any, error) {
	if metadata != nil && safeValues != nil {
		left, err := sharedtelemetry.SafeMetadata(metadata)
		if err != nil {
			return nil, err
		}
		right, err := sharedtelemetry.SafeMetadata(safeValues)
		if err != nil {
			return nil, err
		}
		leftJSON, _ := json.Marshal(left)
		rightJSON, _ := json.Marshal(right)
		if !bytes.Equal(leftJSON, rightJSON) {
			return nil, safe.NewError(safe.ErrorIdentityMismatch, "construct lifecycle telemetry")
		}
		return left, nil
	}
	if safeValues != nil {
		return sharedtelemetry.SafeMetadata(safeValues)
	}
	return sharedtelemetry.SafeMetadata(metadata)
}

func validOptionalOpaqueID(value string) bool {
	return value == "" || sharedtelemetry.ValidOpaqueID(value)
}

func metadataIsSafe(metadata map[string]any) bool {
	clean, err := sharedtelemetry.SafeMetadata(metadata)
	if err != nil {
		return false
	}
	left, err := json.Marshal(clean)
	if err != nil {
		return false
	}
	right, err := json.Marshal(metadata)
	return err == nil && bytes.Equal(left, right)
}

// LifecycleEventLog and LifecycleLog retain separate bounded streams. They
// use TryLock so a slow snapshot never adds producer backpressure; dropped
// records remain observable through Dropped.
type LifecycleEventLog struct {
	mu       sync.RWMutex
	capacity int
	events   []LifecycleEvent
	dropped  atomic.Uint64
	closed   atomic.Bool
}

func NewLifecycleEventLog(capacity int) (*LifecycleEventLog, error) {
	if capacity <= 0 || capacity > maximumLifecycleLogSize {
		return nil, safe.NewError(safe.ErrorInvalidCapacity, "construct lifecycle event log")
	}
	return &LifecycleEventLog{capacity: capacity, events: make([]LifecycleEvent, 0, capacity)}, nil
}

func (l *LifecycleEventLog) Record(input LifecycleEventInput) (LifecycleEvent, error) {
	event, err := NewLifecycleEvent(input)
	if err != nil {
		return LifecycleEvent{}, err
	}
	if l == nil || l.closed.Load() {
		return event, nil
	}
	if !l.mu.TryLock() {
		l.dropped.Add(1)
		return event, nil
	}
	defer l.mu.Unlock()
	if l.closed.Load() {
		l.dropped.Add(1)
		return event, nil
	}
	if len(l.events) == l.capacity {
		copy(l.events, l.events[1:])
		l.events[len(l.events)-1] = cloneLifecycleEvent(event)
		l.dropped.Add(1)
	} else {
		l.events = append(l.events, cloneLifecycleEvent(event))
	}
	return event, nil
}

func (l *LifecycleEventLog) Snapshot() []LifecycleEvent {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]LifecycleEvent, len(l.events))
	for index, event := range l.events {
		result[index] = cloneLifecycleEvent(event)
	}
	return result
}

func (l *LifecycleEventLog) Dropped() uint64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

func (l *LifecycleEventLog) Close() error {
	if l != nil {
		l.mu.Lock()
		l.closed.Store(true)
		l.mu.Unlock()
	}
	return nil
}

type LifecycleLog struct {
	mu       sync.RWMutex
	capacity int
	entries  []LogRecord
	dropped  atomic.Uint64
	closed   atomic.Bool
}

func NewLifecycleLog(capacity int) (*LifecycleLog, error) {
	if capacity <= 0 || capacity > maximumLifecycleLogSize {
		return nil, safe.NewError(safe.ErrorInvalidCapacity, "construct lifecycle log")
	}
	return &LifecycleLog{capacity: capacity, entries: make([]LogRecord, 0, capacity)}, nil
}

func (l *LifecycleLog) Record(input LogRecordInput) (LogRecord, error) {
	record, err := NewLogRecord(input)
	if err != nil {
		return LogRecord{}, err
	}
	if l == nil || l.closed.Load() {
		return record, nil
	}
	if !l.mu.TryLock() {
		l.dropped.Add(1)
		return record, nil
	}
	defer l.mu.Unlock()
	if l.closed.Load() {
		l.dropped.Add(1)
		return record, nil
	}
	if len(l.entries) == l.capacity {
		copy(l.entries, l.entries[1:])
		l.entries[len(l.entries)-1] = cloneLogRecord(record)
		l.dropped.Add(1)
	} else {
		l.entries = append(l.entries, cloneLogRecord(record))
	}
	return record, nil
}

func (l *LifecycleLog) Snapshot() []LogRecord {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]LogRecord, len(l.entries))
	for index, entry := range l.entries {
		result[index] = cloneLogRecord(entry)
	}
	return result
}

func (l *LifecycleLog) Dropped() uint64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

func (l *LifecycleLog) Close() error {
	if l != nil {
		l.mu.Lock()
		l.closed.Store(true)
		l.mu.Unlock()
	}
	return nil
}

func cloneLifecycleEvent(event LifecycleEvent) LifecycleEvent {
	if event.NextRetryAt != nil {
		event.NextRetryAt = safe.CloneTime(event.NextRetryAt)
	}
	if event.Actor != nil {
		actor := *event.Actor
		event.Actor = &actor
	}
	event.Metadata, _ = sharedtelemetry.SafeMetadata(event.Metadata)
	return event
}

func cloneLogRecord(record LogRecord) LogRecord {
	if record.NextRetryAt != nil {
		record.NextRetryAt = safe.CloneTime(record.NextRetryAt)
	}
	record.Metadata, _ = sharedtelemetry.SafeMetadata(record.Metadata)
	return record
}
