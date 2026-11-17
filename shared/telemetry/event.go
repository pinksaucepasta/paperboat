// Package telemetry is the one definition of Paperboat's canonical telemetry
// contract: events, the bounded in-process event log, and the preview/tunnel
// resource envelopes (health, event, log entry, error). The host runtime, the
// edge and the control plane all produce it, so one decoder reads every side.
//
// Producers validate before they emit (NewEvent, the New*Resource constructors);
// readers decode tolerantly and only call Validate on what they choose to trust.
//
// Prior art: tailscale tailcfg (one package, imported by both ends of the wire).
package telemetry

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/shared/health"
	"github.com/pinksaucepasta/paperboat/shared/telemetry/safe"
	"sync"
	"sync/atomic"
	"time"
)

// EventSchemaV1 identifies the canonical event envelope for every producer.
const EventSchemaV1 = "paperboat.edge_event.v1"

// MaximumEventLogSize bounds the retained ring and the producer queue.
const MaximumEventLogSize = 4096

type EventSeverity string

const (
	SeverityDebug EventSeverity = "debug"
	SeverityInfo  EventSeverity = "info"
	SeverityWarn  EventSeverity = "warn"
	SeverityError EventSeverity = "error"
)

type EventOutcome string

const (
	OutcomeSuccess     EventOutcome = "success"
	OutcomeFailed      EventOutcome = "failed"
	OutcomeRejected    EventOutcome = "rejected"
	OutcomeCanceled    EventOutcome = "canceled"
	OutcomeStateChange EventOutcome = "state_change"
)

// SafeIDs is deliberately limited to opaque IDs.  It is not a place for
// hostnames, URLs, email addresses, credentials or user-provided labels.
type SafeIDs struct {
	AccountID     string `json:"account_id,omitempty"`
	ActorID       string `json:"actor_id,omitempty"`
	TunnelID      string `json:"tunnel_id,omitempty"`
	RouteID       string `json:"route_id,omitempty"`
	ConnectorID   string `json:"connector_id,omitempty"`
	DomainID      string `json:"domain_id,omitempty"`
	CertificateID string `json:"certificate_id,omitempty"`
	AssignmentID  string `json:"assignment_id,omitempty"`
	HostID        string `json:"host_id,omitempty"`
	MachineID     string `json:"machine_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	OperationID   string `json:"operation_id,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
	EdgeNodeID    string `json:"edge_node_id,omitempty"`
	ResourceID    string `json:"resource_id,omitempty"`
	ProcessID     string `json:"process_id,omitempty"`
	ConfigID      string `json:"config_id,omitempty"`
}

type Generations struct {
	Config       uint64 `json:"config,omitempty"`
	Route        uint64 `json:"route,omitempty"`
	Assignment   uint64 `json:"assignment,omitempty"`
	Connector    uint64 `json:"connector,omitempty"`
	Process      uint64 `json:"process,omitempty"`
	Session      uint64 `json:"session,omitempty"`
	Installation uint64 `json:"installation,omitempty"`
	Credential   uint64 `json:"credential,omitempty"`
	Certificate  uint64 `json:"certificate,omitempty"`
}

type EventInput struct {
	At            time.Time
	Severity      EventSeverity
	Component     health.Dimension
	Name          string
	Code          string
	Outcome       EventOutcome
	Message       string
	CorrelationID string
	IDs           SafeIDs
	Generations   Generations
	Retry         health.RetryDecision
	NextRetryAt   time.Time
	// The following fields are optional on the low-level edge-compatible event
	// shape. NewLifecycleEvent requires all of them and emits the richer
	// server-owned lifecycle envelope.
	RequestID    string
	ResourceKind string
	ResourceID   string
	OperationID  string
}

type Event struct {
	Schema        string               `json:"schema"`
	At            time.Time            `json:"at"`
	Severity      EventSeverity        `json:"severity"`
	Component     health.Dimension     `json:"component"`
	Name          string               `json:"name"`
	Code          string               `json:"code"`
	Outcome       EventOutcome         `json:"outcome"`
	Message       string               `json:"message"`
	CorrelationID string               `json:"correlation_id"`
	IDs           SafeIDs              `json:"ids"`
	Generations   Generations          `json:"generations"`
	Retry         health.RetryDecision `json:"retry"`
	NextRetryAt   *time.Time           `json:"next_retry_at,omitempty"`
	RequestID     string               `json:"request_id,omitempty"`
	ResourceKind  string               `json:"resource_kind,omitempty"`
	ResourceID    string               `json:"resource_id,omitempty"`
	OperationID   string               `json:"operation_id,omitempty"`
}

func NewEvent(input EventInput) (Event, error) {
	if safe.NormalizeTime(input.At).IsZero() {
		return Event{}, safe.NewError(safe.ErrorInvalidTime, "construct event")
	}
	if !ValidEventSeverity(input.Severity) || !health.ValidDimension(input.Component) || !safe.StableCode(input.Name) || !safe.StableCode(input.Code) || !ValidEventOutcome(input.Outcome) {
		return Event{}, safe.NewError(safe.ErrorInvalidEvent, "construct event")
	}
	if !safe.CorrelationID(input.CorrelationID) {
		return Event{}, safe.NewError(safe.ErrorInvalidID, "construct event")
	}
	if !validSafeIDs(input.IDs) {
		return Event{}, safe.NewError(safe.ErrorInvalidID, "construct event")
	}
	if !health.ValidRetry(input.Retry, input.NextRetryAt) {
		return Event{}, safe.NewError(safe.ErrorInvalidRetry, "construct event")
	}
	if input.RequestID != "" && input.IDs.RequestID != "" && input.RequestID != input.IDs.RequestID {
		return Event{}, safe.NewError(safe.ErrorIdentityMismatch, "construct event")
	}
	if !ValidOptionalLogIdentity(input.RequestID, "request_", "req_") {
		return Event{}, safe.NewError(safe.ErrorInvalidID, "construct event")
	}
	if (input.ResourceKind == "") != (input.ResourceID == "") || (input.ResourceKind != "" && !ValidEventResourceKind(input.ResourceKind)) || (input.ResourceID != "" && !ValidOpaqueID(input.ResourceID)) || !ValidOptionalLogIdentity(input.OperationID, "operation_", "op_") {
		return Event{}, safe.NewError(safe.ErrorInvalidID, "construct event")
	}
	message, err := safe.BoundedString(input.Message, safe.MaximumMessageBytes, true)
	if err != nil {
		return Event{}, err
	}
	event := Event{Schema: EventSchemaV1, At: safe.NormalizeTime(input.At), Severity: input.Severity, Component: input.Component, Name: input.Name, Code: input.Code, Outcome: input.Outcome, Message: message, CorrelationID: input.CorrelationID, IDs: input.IDs, Generations: input.Generations, Retry: input.Retry, RequestID: input.RequestID, ResourceKind: input.ResourceKind, ResourceID: input.ResourceID, OperationID: input.OperationID}
	if event.RequestID == "" && input.IDs.RequestID != "" {
		// Keep the existing nested ID as the canonical low-level JSON shape.
		event.RequestID = ""
	}
	if !input.NextRetryAt.IsZero() {
		nextRetry := safe.NormalizeTime(input.NextRetryAt)
		event.NextRetryAt = &nextRetry
	}
	return event, nil
}

func (e Event) JSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

func (e Event) Validate() error {
	if e.Schema != EventSchemaV1 || e.At.IsZero() || !ValidEventSeverity(e.Severity) || !health.ValidDimension(e.Component) || !safe.StableCode(e.Name) || !safe.StableCode(e.Code) || !ValidEventOutcome(e.Outcome) || !safe.CorrelationID(e.CorrelationID) || !validSafeIDs(e.IDs) || !health.ValidRetry(e.Retry, safe.TimeValue(e.NextRetryAt)) || !safe.StoredText(e.Message, safe.MaximumMessageBytes) {
		return safe.NewError(safe.ErrorInvalidEvent, "validate event")
	}
	if !ValidOptionalLogIdentity(e.RequestID, "request_", "req_") || (e.ResourceKind == "") != (e.ResourceID == "") || (e.ResourceKind != "" && !ValidEventResourceKind(e.ResourceKind)) || (e.ResourceID != "" && !ValidOpaqueID(e.ResourceID)) || !ValidOptionalLogIdentity(e.OperationID, "operation_", "op_") {
		return safe.NewError(safe.ErrorInvalidID, "validate event")
	}
	return nil
}

func ValidEventSeverity(value EventSeverity) bool {
	return value == SeverityDebug || value == SeverityInfo || value == SeverityWarn || value == SeverityError
}

func ValidEventOutcome(value EventOutcome) bool {
	return value == OutcomeSuccess || value == OutcomeFailed || value == OutcomeRejected || value == OutcomeCanceled || value == OutcomeStateChange
}

// validSafeIDs applies the opaque-ID rule to every field: IDs are minted by the
// control plane, the edge and the host (prefixed, UUID or hex), so the rule bounds
// and secret-checks them without interpreting their spelling.
func validSafeIDs(ids SafeIDs) bool {
	for _, id := range []string{ids.AccountID, ids.ActorID, ids.TunnelID, ids.RouteID, ids.ConnectorID, ids.DomainID, ids.CertificateID, ids.AssignmentID, ids.HostID, ids.MachineID, ids.SessionID, ids.OperationID, ids.RequestID, ids.EdgeNodeID, ids.ResourceID, ids.ProcessID, ids.ConfigID} {
		if id != "" && !safe.OpaqueID(id) {
			return false
		}
	}
	return true
}

// EventLog is a bounded, nonblocking producer path backed by an owned worker.
// Record never waits for a sink or for a slow reader.  A full queue increments
// DroppedEvents and discards the event safely.
type EventLog struct {
	observer func(Event)
	mu       sync.RWMutex
	gate     sync.RWMutex
	capacity int
	events   []Event
	queue    chan eventItem
	commands chan flushCommand
	stop     chan struct{}
	done     chan struct{}
	closed   atomic.Bool
	dropped  atomic.Uint64
	accepted atomic.Uint64
	closeMu  sync.Mutex
}

type eventItem struct {
	event   Event
	barrier chan struct{}
}

type flushCommand struct{ done chan struct{} }

func NewEventLog(capacity int) (*EventLog, error) {
	queueCapacity := capacity * 2
	if queueCapacity < capacity || queueCapacity > MaximumEventLogSize {
		queueCapacity = MaximumEventLogSize
	}
	return NewEventLogWithQueue(capacity, queueCapacity)
}

// NewEventLogWithQueue is useful when callers want a small producer queue but
// a larger retained ring.  Both bounds are explicit and finite.
func NewEventLogWithQueue(capacity, queueCapacity int) (*EventLog, error) {
	if capacity <= 0 || capacity > MaximumEventLogSize || queueCapacity <= 0 || queueCapacity > MaximumEventLogSize {
		return nil, safe.NewError(safe.ErrorInvalidCapacity, "construct event log")
	}
	log := &EventLog{capacity: capacity, events: make([]Event, 0, capacity), queue: make(chan eventItem, queueCapacity), commands: make(chan flushCommand), stop: make(chan struct{}), done: make(chan struct{})}
	go log.run()
	return log, nil
}

func (l *EventLog) run() {
	defer close(l.done)
	for {
		select {
		case item := <-l.queue:
			if item.barrier != nil {
				close(item.barrier)
				continue
			}
			l.append(item.event)
		case command := <-l.commands:
			l.drain()
			close(command.done)
		case <-l.stop:
			l.drain()
			return
		}
	}
}

func (l *EventLog) drain() {
	for {
		select {
		case item := <-l.queue:
			if item.barrier != nil {
				close(item.barrier)
			} else {
				l.append(item.event)
			}
		default:
			return
		}
	}
}

// SetObserver installs a nonblocking metadata exporter. It observes the same
// accepted events as the local ring, outside its lock.
func (l *EventLog) SetObserver(fn func(Event)) {
	l.mu.Lock()
	l.observer = fn
	l.mu.Unlock()
}

func (l *EventLog) append(event Event) {
	l.mu.Lock()
	observer := l.observer
	defer func() {
		l.mu.Unlock()
		if observer != nil {
			observer(cloneEvent(event))
		}
	}()
	if len(l.events) == l.capacity {
		copy(l.events, l.events[1:])
		l.events[len(l.events)-1] = cloneEvent(event)
		return
	}
	l.events = append(l.events, cloneEvent(event))
}

// Record validates input and enqueues the event without ever waiting for the
// worker, a sink or a slow reader. A full queue, a closing gate or a closed log
// drops it and counts the drop.
func (l *EventLog) Record(input EventInput) (Event, error) {
	event, _, err := l.TryRecord(input)
	return event, err
}

// TryRecord is Record that also reports whether the event was queued. A busy
// ring mutex only delays the worker, so the event is still queued.
func (l *EventLog) TryRecord(input EventInput) (Event, bool, error) {
	event, err := NewEvent(input)
	if err != nil {
		return Event{}, false, err
	}
	if l == nil {
		return event, false, nil
	}
	if !l.gate.TryRLock() {
		l.dropped.Add(1)
		return event, false, nil
	}
	defer l.gate.RUnlock()
	if l.closed.Load() {
		l.dropped.Add(1)
		return event, false, nil
	}
	select {
	case l.queue <- eventItem{event: event}:
		if accepted := l.accepted.Add(1); accepted > uint64(l.capacity) {
			// Retention is accounted for at acceptance time so callers can
			// observe drops without waiting for the worker to run.
			l.dropped.Add(1)
		}
		return event, true, nil
	default:
		l.dropped.Add(1)
		return event, false, nil
	}
}

// Flush waits until events accepted before the call are retained.  Unlike
// Record, it may wait for the owned worker and honors cancellation.
func (l *EventLog) Flush(ctx context.Context) error {
	if l == nil || l.closed.Load() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	command := flushCommand{done: make(chan struct{})}
	select {
	case l.commands <- command:
	case <-ctx.Done():
		return ctx.Err()
	case <-l.done:
		return nil
	}
	select {
	case <-command.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *EventLog) Snapshot() []Event {
	if l == nil {
		return nil
	}
	_ = l.Flush(context.Background())
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := make([]Event, len(l.events))
	for index, event := range l.events {
		result[index] = cloneEvent(event)
	}
	return result
}

func (l *EventLog) DroppedEvents() uint64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

func (l *EventLog) Close() error {
	if l == nil {
		return nil
	}
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	if l.closed.Load() {
		<-l.done
		return nil
	}
	l.gate.Lock()
	l.closed.Store(true)
	close(l.stop)
	l.gate.Unlock()
	<-l.done
	return nil
}

func cloneEvent(event Event) Event {
	event.NextRetryAt = safe.CloneTime(event.NextRetryAt)
	return event
}
