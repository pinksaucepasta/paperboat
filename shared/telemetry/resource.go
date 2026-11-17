package telemetry

import (
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/shared/health"
	"github.com/pinksaucepasta/paperboat/shared/telemetry/safe"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ContractSchemaV1 is the language-neutral resource schema shared by the
// server, host runtime, edge and dashboard.
const ContractSchemaV1 = "paperboat.preview-tunnel/v1"

const (
	KindHealth         = "health"
	KindEvent          = "event"
	KindLog            = "log_entry"
	KindError          = "error"
	KindLifecycleEvent = "lifecycle_event"
	KindLifecycleLog   = "lifecycle_log"
)

const DimensionControl health.Dimension = "control"

var (
	resourceIDPattern     = regexp.MustCompile(`^[A-Za-z0-9._:-]{3,128}$`)
	healthResourceKindSet = map[string]struct{}{
		"preview_lease": {}, "tunnel": {}, "route": {}, "domain_binding": {}, "connector": {},
	}
	actorTypeSet              = map[string]struct{}{"user": {}, "host": {}, "system": {}, "edge": {}}
	metadataKeyPattern        = regexp.MustCompile(`(?i)(token|secret|private[_-]?key|authorization|password|cookie)`)
	canonicalEventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,79}$`)
	componentPattern          = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
)

type CanonicalHealthDimension struct {
	Status health.HealthStatus `json:"status"`
	Code   string              `json:"code"`
}

type CanonicalHealthDimensions struct {
	Service     CanonicalHealthDimension `json:"service"`
	Edge        CanonicalHealthDimension `json:"edge"`
	Config      CanonicalHealthDimension `json:"config"`
	Route       CanonicalHealthDimension `json:"route"`
	Origin      CanonicalHealthDimension `json:"origin"`
	DNS         CanonicalHealthDimension `json:"dns"`
	Certificate CanonicalHealthDimension `json:"certificate"`
	Access      CanonicalHealthDimension `json:"access"`
	Update      CanonicalHealthDimension `json:"update"`
}

func (d CanonicalHealthDimensions) Get(dimension health.Dimension) CanonicalHealthDimension {
	switch dimension {
	case health.DimensionService:
		return d.Service
	case health.DimensionEdge:
		return d.Edge
	case health.DimensionConfig:
		return d.Config
	case health.DimensionRoute:
		return d.Route
	case health.DimensionOrigin:
		return d.Origin
	case health.DimensionDNS:
		return d.DNS
	case health.DimensionCertificate:
		return d.Certificate
	case health.DimensionAccess:
		return d.Access
	case health.DimensionUpdate:
		return d.Update
	default:
		return CanonicalHealthDimension{}
	}
}

type HealthResource struct {
	Schema        string                    `json:"schema"`
	Kind          string                    `json:"kind"`
	ResourceKind  string                    `json:"resource_kind"`
	ResourceID    string                    `json:"resource_id"`
	OverallCode   string                    `json:"overall_code"`
	Dimensions    CanonicalHealthDimensions `json:"dimensions"`
	Summary       string                    `json:"summary"`
	Since         time.Time                 `json:"since"`
	Retrying      bool                      `json:"retrying"`
	NextRetryAt   *time.Time                `json:"next_retry_at,omitempty"`
	RepairAction  string                    `json:"repair_action"`
	CorrelationID string                    `json:"correlation_id"`
}

// HealthResourceBinding names the resource a snapshot is projected onto.
type HealthResourceBinding struct {
	ResourceKind  string
	ResourceID    string
	CorrelationID string
}

// ProjectHealthResource projects one tracker snapshot onto binding's resource.
func ProjectHealthResource(snapshot health.HealthSnapshot, binding HealthResourceBinding) (HealthResource, error) {
	if snapshot.Schema != health.HealthSchemaV1 {
		return HealthResource{}, safe.NewError(safe.ErrorInvalidObservation, "project health resource")
	}
	return NewHealthResource(HealthResourceInput{ResourceKind: binding.ResourceKind, ResourceID: binding.ResourceID, Snapshot: snapshot, CorrelationID: binding.CorrelationID})
}

type HealthResourceInput struct {
	ResourceKind  string
	ResourceID    string
	Snapshot      health.HealthSnapshot
	CorrelationID string
}

func NewHealthResource(input HealthResourceInput) (HealthResource, error) {
	if !validHealthResourceKind(input.ResourceKind) || !ValidOpaqueID(input.ResourceID) {
		return HealthResource{}, safe.NewError(safe.ErrorInvalidID, "construct health resource")
	}
	snapshot := input.Snapshot
	if snapshot.Schema != "" && snapshot.Schema != health.HealthSchemaV1 {
		return HealthResource{}, safe.NewError(safe.ErrorInvalidObservation, "construct health resource")
	}
	if safe.NormalizeTime(snapshot.UpdatedAt).IsZero() || safe.NormalizeTime(snapshot.Overall.Since).IsZero() || !safe.StableCode(snapshot.Overall.Code) {
		return HealthResource{}, safe.NewError(safe.ErrorInvalidObservation, "construct health resource")
	}
	correlationID := input.CorrelationID
	if correlationID == "" {
		correlationID = snapshot.Overall.CorrelationID
	}
	if input.CorrelationID != "" && snapshot.Overall.CorrelationID != "" && input.CorrelationID != snapshot.Overall.CorrelationID {
		return HealthResource{}, safe.NewError(safe.ErrorInvalidID, "construct health resource")
	}
	if !safe.CorrelationID(correlationID) {
		return HealthResource{}, safe.NewError(safe.ErrorInvalidID, "construct health resource")
	}
	summary, err := safe.BoundedString(snapshot.Overall.Summary, safe.MaximumSummaryBytes, true)
	if err != nil {
		return HealthResource{}, err
	}
	repairAction, err := safe.BoundedString(snapshot.Overall.RepairAction, safe.MaximumRepairBytes, true)
	if err != nil {
		return HealthResource{}, err
	}
	var nextRetryAt *time.Time
	if snapshot.Overall.NextRetryAt != nil {
		value := safe.NormalizeTime(*snapshot.Overall.NextRetryAt)
		if value.IsZero() {
			return HealthResource{}, safe.NewError(safe.ErrorInvalidRetry, "construct health resource")
		}
		nextRetryAt = &value
	}
	resource := HealthResource{Schema: ContractSchemaV1, Kind: KindHealth, ResourceKind: input.ResourceKind, ResourceID: input.ResourceID, OverallCode: snapshot.Overall.Code, Summary: summary, Since: safe.NormalizeTime(snapshot.Overall.Since), Retrying: snapshot.Overall.Retry == health.RetryScheduled, NextRetryAt: nextRetryAt, RepairAction: repairAction, CorrelationID: correlationID}
	for _, dimension := range health.Dimensions() {
		state := snapshot.Dimensions.Get(dimension)
		resource.Dimensions.set(dimension, CanonicalHealthDimension{Status: state.Status, Code: state.Code})
	}
	if err := resource.Validate(); err != nil {
		return HealthResource{}, err
	}
	return resource, nil
}

func (d *CanonicalHealthDimensions) set(dimension health.Dimension, value CanonicalHealthDimension) {
	switch dimension {
	case health.DimensionService:
		d.Service = value
	case health.DimensionEdge:
		d.Edge = value
	case health.DimensionConfig:
		d.Config = value
	case health.DimensionRoute:
		d.Route = value
	case health.DimensionOrigin:
		d.Origin = value
	case health.DimensionDNS:
		d.DNS = value
	case health.DimensionCertificate:
		d.Certificate = value
	case health.DimensionAccess:
		d.Access = value
	case health.DimensionUpdate:
		d.Update = value
	}
}

func (r HealthResource) Validate() error {
	if r.Schema != ContractSchemaV1 || r.Kind != KindHealth || !validHealthResourceKind(r.ResourceKind) || !ValidOpaqueID(r.ResourceID) || !safe.StableCode(r.OverallCode) || r.Since.IsZero() || !safe.CorrelationID(r.CorrelationID) || !safe.StoredText(r.Summary, safe.MaximumSummaryBytes) || !safe.StoredText(r.RepairAction, safe.MaximumRepairBytes) || r.Retrying != (r.NextRetryAt != nil) || (r.NextRetryAt != nil && r.NextRetryAt.IsZero()) {
		return safe.NewError(safe.ErrorInvalidObservation, "validate health resource")
	}
	for _, dimension := range health.Dimensions() {
		state := r.Dimensions.Get(dimension)
		if !health.ValidHealthStatus(state.Status) || !safe.StableCode(state.Code) {
			return safe.NewError(safe.ErrorInvalidObservation, "validate health resource")
		}
	}
	return nil
}

func (r HealthResource) JSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

type CanonicalActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type EventActor = CanonicalActor

type CanonicalEventResource struct {
	Schema        string         `json:"schema"`
	Kind          string         `json:"kind"`
	ID            string         `json:"id"`
	Cursor        string         `json:"cursor"`
	EventType     string         `json:"event_type"`
	ResourceKind  string         `json:"resource_kind"`
	ResourceID    string         `json:"resource_id"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Actor         CanonicalActor `json:"actor"`
	CorrelationID string         `json:"correlation_id"`
	SafeMetadata  map[string]any `json:"safe_metadata"`
}

type CanonicalEventInput struct {
	ID            string
	Cursor        string
	EventType     string
	ResourceKind  string
	ResourceID    string
	OccurredAt    time.Time
	ActorType     string
	ActorID       string
	CorrelationID string
	SafeMetadata  map[string]any
}

func NewCanonicalEvent(input CanonicalEventInput) (CanonicalEventResource, error) {
	if !ValidOpaqueID(input.ID) || !ValidOpaqueID(input.Cursor) || !canonicalEventTypePattern.MatchString(input.EventType) || !ValidEventResourceKind(input.ResourceKind) || !ValidOpaqueID(input.ResourceID) || safe.NormalizeTime(input.OccurredAt).IsZero() || !ValidActorType(input.ActorType) || !ValidOpaqueID(input.ActorID) || !safe.CorrelationID(input.CorrelationID) {
		return CanonicalEventResource{}, safe.NewError(safe.ErrorInvalidEvent, "construct canonical event")
	}
	metadata, err := SafeMetadata(input.SafeMetadata)
	if err != nil {
		return CanonicalEventResource{}, err
	}
	event := CanonicalEventResource{Schema: ContractSchemaV1, Kind: KindEvent, ID: input.ID, Cursor: input.Cursor, EventType: input.EventType, ResourceKind: input.ResourceKind, ResourceID: input.ResourceID, OccurredAt: safe.NormalizeTime(input.OccurredAt), Actor: CanonicalActor{Type: input.ActorType, ID: input.ActorID}, CorrelationID: input.CorrelationID, SafeMetadata: metadata}
	return event, event.Validate()
}

func (e CanonicalEventResource) Validate() error {
	if e.Schema != ContractSchemaV1 || e.Kind != KindEvent || !ValidOpaqueID(e.ID) || !ValidOpaqueID(e.Cursor) || !canonicalEventTypePattern.MatchString(e.EventType) || !ValidEventResourceKind(e.ResourceKind) || !ValidOpaqueID(e.ResourceID) || e.OccurredAt.IsZero() || !ValidActorType(e.Actor.Type) || !ValidOpaqueID(e.Actor.ID) || !safe.CorrelationID(e.CorrelationID) {
		return safe.NewError(safe.ErrorInvalidEvent, "validate canonical event")
	}
	_, err := SafeMetadata(e.SafeMetadata)
	return err
}

func (e CanonicalEventResource) JSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// EventResource is an alias-friendly name for consumers that do not want the
// longer canonical type name.
type EventResource = CanonicalEventResource

func NewEventResource(input CanonicalEventInput) (EventResource, error) {
	return NewCanonicalEvent(input)
}

type LogEntry struct {
	Schema        string         `json:"schema"`
	Kind          string         `json:"kind"`
	ID            string         `json:"id"`
	TunnelID      string         `json:"tunnel_id,omitempty"`
	PreviewID     string         `json:"preview_id,omitempty"`
	RouteID       string         `json:"route_id,omitempty"`
	ConnectorID   string         `json:"connector_id,omitempty"`
	SessionID     string         `json:"session_id,omitempty"`
	RequestID     string         `json:"request_id,omitempty"`
	OperationID   string         `json:"operation_id,omitempty"`
	ResourceKind  string         `json:"resource_kind,omitempty"`
	ResourceID    string         `json:"resource_id,omitempty"`
	Level         EventSeverity  `json:"level"`
	Component     string         `json:"component"`
	Code          string         `json:"code"`
	Message       string         `json:"message"`
	Metadata      map[string]any `json:"metadata"`
	CorrelationID string         `json:"correlation_id"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Cursor        string         `json:"cursor"`
}

type LogEntryInput struct {
	ID, TunnelID, PreviewID, RouteID, ConnectorID, SessionID string
	RequestID, OperationID, ResourceKind, ResourceID         string
	Level                                                    EventSeverity
	Component, Code, Message, CorrelationID, Cursor          string
	Metadata                                                 map[string]any
	OccurredAt                                               time.Time
}

func NewLogEntry(input LogEntryInput) (LogEntry, error) {
	if !ValidOpaqueID(input.ID) || !ValidEventSeverity(input.Level) || !componentPattern.MatchString(input.Component) || !canonicalEventTypePattern.MatchString(input.Code) || !safe.CorrelationID(input.CorrelationID) || !ValidOpaqueID(input.Cursor) || safe.NormalizeTime(input.OccurredAt).IsZero() || !ValidOptionalLogIdentity(input.RequestID, "request_", "req_") || !ValidOptionalLogIdentity(input.OperationID, "operation_", "op_") || (input.ResourceKind != "" && !ValidEventResourceKind(input.ResourceKind)) || (input.ResourceID != "" && !ValidOpaqueID(input.ResourceID)) {
		return LogEntry{}, safe.NewError(safe.ErrorInvalidEvent, "construct log entry")
	}
	message, err := safe.BoundedString(input.Message, 1000, true)
	if err != nil {
		return LogEntry{}, err
	}
	metadata, err := SafeMetadata(input.Metadata)
	if err != nil {
		return LogEntry{}, err
	}
	entry := LogEntry{Schema: ContractSchemaV1, Kind: KindLog, ID: input.ID, TunnelID: input.TunnelID, PreviewID: input.PreviewID, RouteID: input.RouteID, ConnectorID: input.ConnectorID, SessionID: input.SessionID, RequestID: input.RequestID, OperationID: input.OperationID, ResourceKind: input.ResourceKind, ResourceID: input.ResourceID, Level: input.Level, Component: input.Component, Code: input.Code, Message: message, Metadata: metadata, CorrelationID: input.CorrelationID, OccurredAt: safe.NormalizeTime(input.OccurredAt), Cursor: input.Cursor}
	return entry, entry.Validate()
}

func (e LogEntry) Validate() error {
	if e.Schema != ContractSchemaV1 || e.Kind != KindLog || !ValidOpaqueID(e.ID) || !ValidEventSeverity(e.Level) || !componentPattern.MatchString(e.Component) || !canonicalEventTypePattern.MatchString(e.Code) || e.Message == "" || !safe.CorrelationID(e.CorrelationID) || e.OccurredAt.IsZero() || !ValidOpaqueID(e.Cursor) || !ValidOptionalLogIdentity(e.RequestID, "request_", "req_") || !ValidOptionalLogIdentity(e.OperationID, "operation_", "op_") || (e.ResourceKind != "" && !ValidEventResourceKind(e.ResourceKind)) || (e.ResourceID != "" && !ValidOpaqueID(e.ResourceID)) {
		return safe.NewError(safe.ErrorInvalidEvent, "validate log entry")
	}
	for _, id := range []string{e.TunnelID, e.PreviewID, e.RouteID, e.ConnectorID, e.SessionID} {
		if id != "" && !ValidOpaqueID(id) {
			return safe.NewError(safe.ErrorInvalidID, "validate log entry")
		}
	}
	_, err := SafeMetadata(e.Metadata)
	return err
}

type ErrorResource struct {
	Schema        string           `json:"schema"`
	Kind          string           `json:"kind"`
	Code          string           `json:"code"`
	Component     health.Dimension `json:"component"`
	Message       string           `json:"message"`
	Outcome       string           `json:"outcome"`
	Retryable     bool             `json:"retryable"`
	RetryAt       *time.Time       `json:"retry_at"`
	RepairAction  string           `json:"repair_action"`
	RequestID     string           `json:"request_id"`
	CorrelationID string           `json:"correlation_id"`
}

type ErrorResourceInput struct {
	Code          string
	Component     health.Dimension
	Message       string
	Outcome       string
	Retryable     bool
	RetryAt       time.Time
	RepairAction  string
	RequestID     string
	CorrelationID string
}

func NewErrorResource(input ErrorResourceInput) (ErrorResource, error) {
	message, err := safe.BoundedString(input.Message, 1000, true)
	if err != nil {
		return ErrorResource{}, err
	}
	repair, err := safe.BoundedString(input.RepairAction, 1000, true)
	if err != nil {
		return ErrorResource{}, err
	}
	if !safe.StableCode(input.Code) || !validErrorComponent(input.Component) || (input.Outcome != "unchanged" && input.Outcome != "changed" && input.Outcome != "uncertain") || !ValidOptionalLogIdentity(input.RequestID, "request_", "req_") || !ValidOptionalLogIdentity(input.CorrelationID, "cor_", "corr_", "correlation_", "request_", "support_") || input.RequestID == "" || input.CorrelationID == "" {
		return ErrorResource{}, safe.NewError(safe.ErrorInvalidEvent, "construct error resource")
	}
	if input.Retryable != !input.RetryAt.IsZero() {
		return ErrorResource{}, safe.NewError(safe.ErrorInvalidRetry, "construct error resource")
	}
	result := ErrorResource{Schema: ContractSchemaV1, Kind: KindError, Code: input.Code, Component: input.Component, Message: message, Outcome: input.Outcome, Retryable: input.Retryable, RepairAction: repair, RequestID: input.RequestID, CorrelationID: input.CorrelationID}
	if !input.RetryAt.IsZero() {
		retryAt := safe.NormalizeTime(input.RetryAt)
		result.RetryAt = &retryAt
	}
	return result, result.Validate()
}

func (e ErrorResource) Validate() error {
	if e.Schema != ContractSchemaV1 || e.Kind != KindError || !safe.StableCode(e.Code) || !validErrorComponent(e.Component) || (e.Outcome != "unchanged" && e.Outcome != "changed" && e.Outcome != "uncertain") || e.Message == "" || e.RepairAction == "" || !ValidOptionalLogIdentity(e.RequestID, "request_", "req_") || !ValidOptionalLogIdentity(e.CorrelationID, "cor_", "corr_", "correlation_", "request_", "support_") || e.RequestID == "" || e.CorrelationID == "" || e.Retryable != (e.RetryAt != nil) {
		return safe.NewError(safe.ErrorInvalidEvent, "validate error resource")
	}
	return nil
}

func validHealthResourceKind(value string) bool {
	_, ok := healthResourceKindSet[value]
	return ok
}

func ValidEventResourceKind(value string) bool {
	return validHealthResourceKind(value) || value == "config_generation" || value == "operation"
}

func validErrorComponent(value health.Dimension) bool {
	return health.ValidDimension(value) || value == DimensionControl
}

func ValidActorType(value string) bool {
	_, ok := actorTypeSet[value]
	return ok
}

// ValidOpaqueID is deliberately stricter than the shared JSON schema's broad
// ID syntax. The schema must accept IDs emitted by other implementations, but
// server constructors must not turn a hostname, URL, credential, or control
// string into durable telemetry identity.
func ValidOpaqueID(value string) bool {
	if !resourceIDPattern.MatchString(value) || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n\t/\\?#@%") {
		return false
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"authorization", "password", "passwd", "secret", "token", "credential", "cookie", "private_key", "private-key"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	// Dotted values without a Paperboat ID namespace are almost certainly a
	// hostname. Namespaced opaque IDs may contain a dot because the external
	// contract permits it, although producers should normally avoid one.
	if strings.Contains(value, ".") && !hasKnownOpaqueNamespace(value) {
		return false
	}
	return true
}

func hasKnownOpaqueNamespace(value string) bool {
	for _, prefix := range []string{
		"account_", "actor_", "assignment_", "certificate_", "connector_", "cor_", "corr_", "correlation_", "cur_", "machine_", "domain_", "edge_", "err_", "event_", "evt_", "gen_", "host_", "log_", "machine_", "op_", "operation_", "preview_", "prv_", "req_", "request_", "resource_", "route_", "session_", "tunnel_", "tun_",
	} {
		if strings.HasPrefix(strings.ToLower(value), prefix) {
			return true
		}
	}
	return false
}

func ValidOptionalLogIdentity(value string, prefixes ...string) bool {
	if value == "" {
		return true
	}
	if !ValidOpaqueID(value) {
		return false
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(strings.ToLower(value), prefix) {
			return true
		}
	}
	return false
}

func SafeMetadata(input map[string]any) (map[string]any, error) {
	if len(input) > maximumMetadataItems {
		return nil, safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
	}
	result, err := sanitizeMetadataMap(input, 0)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func sanitizeMetadataMap(input map[string]any, depth int) (map[string]any, error) {
	if depth > maximumMetadataDepth || len(input) > maximumMetadataItems {
		return nil, safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
	}
	result := make(map[string]any, len(input))
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" || len(key) > 128 || !utf8.ValidString(key) || strings.TrimSpace(key) != key || strings.ContainsAny(key, "\x00\r\n\t") || metadataKeyPattern.MatchString(key) {
			return nil, safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
		}
		value, err := sanitizeMetadataValue(input[key], depth+1, key)
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}

func sanitizeMetadataValue(value any, depth int, key string) (any, error) {
	if depth > maximumMetadataDepth {
		return nil, safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
	}
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		return safeMetadataString(typed, key)
	case json.Number:
		return safeJSONNumber(typed)
	case bool:
		return typed, nil
	case int:
		return typed, nil
	case int8:
		return typed, nil
	case int16:
		return typed, nil
	case int32:
		return typed, nil
	case int64:
		return typed, nil
	case uint:
		return typed, nil
	case uint8:
		return typed, nil
	case uint16:
		return typed, nil
	case uint32:
		return typed, nil
	case uint64:
		return typed, nil
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return nil, safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
		}
		return typed, nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil, safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
		}
		return typed, nil
	case []any:
		if len(typed) > maximumMetadataItems {
			return nil, safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
		}
		result := make([]any, len(typed))
		for index, item := range typed {
			clean, err := sanitizeMetadataValue(item, depth+1, "")
			if err != nil {
				return nil, err
			}
			result[index] = clean
		}
		return result, nil
	case map[string]any:
		return sanitizeMetadataMap(typed, depth)
	case []string:
		if len(typed) > maximumMetadataItems {
			return nil, safe.NewError(safe.ErrorInvalidString, "construct safe metadata")
		}
		result := make([]any, len(typed))
		for index, item := range typed {
			clean, err := sanitizeMetadataValue(item, depth+1, "")
			if err != nil {
				return nil, err
			}
			result[index] = clean
		}
		return result, nil
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for itemKey, item := range typed {
			converted[itemKey] = item
		}
		return sanitizeMetadataMap(converted, depth)
	default:
		return nil, safe.NewError(safe.ErrorUnsupportedValue, "construct safe metadata")
	}
}

// ValidEventType reports whether value is a canonical lifecycle event type.
func ValidEventType(value string) bool { return canonicalEventTypePattern.MatchString(value) }

// ValidResourceID reports whether value has the shared resource-ID syntax.
func ValidResourceID(value string) bool { return resourceIDPattern.MatchString(value) }

// EventResourceBinding names the durable resource, cursor and actor an event is projected onto.
type EventResourceBinding struct {
	ID           string
	Cursor       string
	ResourceKind string
	ResourceID   string
	Actor        EventActor
}

// ProjectEventResource projects one event onto binding as an event resource.
func ProjectEventResource(event Event, binding EventResourceBinding) (EventResource, error) {
	if event.Schema != EventSchemaV1 || !safe.StableCode(event.Name) || !validWireValue(event.CorrelationID, 3, 128) || event.At.IsZero() {
		return EventResource{}, safe.NewError(safe.ErrorInvalidEvent, "project event resource")
	}
	if !ValidEventResourceKind(binding.ResourceKind) || !validWireValue(binding.ID, 3, 128) || !validWireValue(binding.Cursor, 1, 1000) || !validWireValue(binding.ResourceID, 3, 128) || !ValidActorType(binding.Actor.Type) || !validWireValue(binding.Actor.ID, 3, 128) {
		return EventResource{}, safe.NewError(safe.ErrorInvalidID, "project event resource")
	}
	metadata := make(map[string]any, 14)
	addSafeMetadata(metadata, "account_id", event.IDs.AccountID)
	addSafeMetadata(metadata, "actor_id", event.IDs.ActorID)
	addSafeMetadata(metadata, "tunnel_id", event.IDs.TunnelID)
	addSafeMetadata(metadata, "route_id", event.IDs.RouteID)
	addSafeMetadata(metadata, "connector_id", event.IDs.ConnectorID)
	addSafeMetadata(metadata, "domain_id", event.IDs.DomainID)
	addSafeMetadata(metadata, "session_id", event.IDs.SessionID)
	addSafeMetadata(metadata, "operation_id", event.IDs.OperationID)
	addSafeMetadata(metadata, "request_id", event.IDs.RequestID)
	addSafeMetadata(metadata, "edge_node_id", event.IDs.EdgeNodeID)
	addSafeMetadata(metadata, "certificate_id", event.IDs.CertificateID)
	addSafeMetadata(metadata, "assignment_id", event.IDs.AssignmentID)
	addSafeMetadata(metadata, "host_id", event.IDs.HostID)
	addSafeMetadata(metadata, "machine_id", event.IDs.MachineID)
	addGenerationMetadata(metadata, "config_generation", event.Generations.Config)
	addGenerationMetadata(metadata, "route_generation", event.Generations.Route)
	addGenerationMetadata(metadata, "assignment_generation", event.Generations.Assignment)
	addGenerationMetadata(metadata, "connector_generation", event.Generations.Connector)
	addGenerationMetadata(metadata, "process_generation", event.Generations.Process)
	addGenerationMetadata(metadata, "session_generation", event.Generations.Session)
	metadata, err := SafeMetadata(metadata)
	if err != nil {
		return EventResource{}, err
	}
	// Structured opaque IDs are explicitly safe and remain useful to callers.
	// The free-form redaction policy still applies to every other metadata
	// value, including text that happens to contain an ID-looking token.
	restoreSafeIDMetadata(metadata, event.IDs)
	result := EventResource{
		Schema: ContractSchemaV1, Kind: KindEvent, ID: binding.ID, Cursor: binding.Cursor,
		EventType: event.Name, ResourceKind: binding.ResourceKind, ResourceID: binding.ResourceID,
		OccurredAt: safe.NormalizeTime(event.At), Actor: binding.Actor, CorrelationID: event.CorrelationID,
		SafeMetadata: metadata,
	}
	return result, result.Validate()
}

func restoreSafeIDMetadata(metadata map[string]any, ids SafeIDs) {
	for name, value := range map[string]string{
		"account_id": ids.AccountID, "actor_id": ids.ActorID, "tunnel_id": ids.TunnelID,
		"route_id": ids.RouteID, "connector_id": ids.ConnectorID, "domain_id": ids.DomainID,
		"certificate_id": ids.CertificateID, "assignment_id": ids.AssignmentID, "host_id": ids.HostID,
		"machine_id": ids.MachineID, "session_id": ids.SessionID, "operation_id": ids.OperationID,
		"request_id": ids.RequestID, "edge_node_id": ids.EdgeNodeID,
	} {
		if value != "" {
			metadata[name] = value
		}
	}
}

func addSafeMetadata(metadata map[string]any, name, value string) {
	if value != "" {
		metadata[name] = value
	}
}

func addGenerationMetadata(metadata map[string]any, name string, value uint64) {
	if value != 0 {
		metadata[name] = value
	}
}

func validWireValue(value string, minimum, maximum int) bool {
	if !utf8.ValidString(value) || len(value) < minimum || len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsAny(value, "\x00\r\n")
}
