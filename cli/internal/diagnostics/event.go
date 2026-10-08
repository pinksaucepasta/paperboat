package diagnostics

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

const EventSchemaV1 = "paperboat.diagnostic-event/v1"

var ErrInvalid = errors.New("invalid diagnostic event")

// Filesystem validation keeps a stable public error while fault projection can
// still inspect the original OS cause without formatting paths or values.
type diagnosticValidationError struct{ cause error }

func (*diagnosticValidationError) Error() string           { return ErrInvalid.Error() }
func (e *diagnosticValidationError) Unwrap() []error       { return []error{ErrInvalid, e.cause} }
func (*diagnosticValidationError) DiagnosticStage() string { return "diagnostic_storage" }
func (*diagnosticValidationError) DiagnosticCode() string  { return "diagnostic_storage_unavailable" }
func invalidDiagnostic(cause error) error {
	if cause == nil {
		return ErrInvalid
	}
	return &diagnosticValidationError{cause: cause}
}

var allowedFields = map[string]bool{
	"capability": true, "generation": true, "operation": true, "outcome": true,
	"path_category": true, "phase": true, "reason": true, "relay_region": true,
	"retry_class": true, "state": true, "transport": true,
	"cause": true, "error_type": true, "error_chain": true,
	"component": true, "service_component": true, "errno": true, "http_status": true,
	"source_file": true, "source_function": true, "source_line": true,
	"sdk_submissions_dropped": true, "sdk_http_failures": true,
}

type Event struct {
	Schema           string            `json:"schema"`
	At               time.Time         `json:"at"`
	Category         string            `json:"category"`
	Code             string            `json:"code"`
	Severity         string            `json:"severity"`
	SupportReference string            `json:"support_reference,omitempty"`
	Fields           map[string]string `json:"fields,omitempty"`
}

func NewEvent(at time.Time, category, code, severity string, fields map[string]string) (Event, error) {
	event := Event{Schema: EventSchemaV1, At: at.UTC(), Category: category, Code: code, Severity: severity, Fields: cloneFields(fields)}
	if event.Validate() != nil {
		return Event{}, ErrInvalid
	}
	return event, nil
}

func NewEventWithSupportReference(at time.Time, category, code, severity, reference string, fields map[string]string) (Event, error) {
	event, err := NewEvent(at, category, code, severity, fields)
	if err != nil {
		return Event{}, err
	}
	event.SupportReference = reference
	if event.Validate() != nil {
		return Event{}, ErrInvalid
	}
	return event, nil
}

func (e Event) Validate() error {
	if e.Schema != EventSchemaV1 || e.At.IsZero() || e.At.Location() != time.UTC || !safeIdentifier(e.Category, 32) || !safeIdentifier(e.Code, 64) || e.Severity != "info" && e.Severity != "warning" && e.Severity != "error" || len(e.Fields) > 16 || e.SupportReference != "" && !supportref.Valid(e.SupportReference) {
		return ErrInvalid
	}
	for key, value := range e.Fields {
		limit := 128
		if key == "error_chain" {
			limit = 1024
		}
		if !allowedFields[key] || !safeIdentifier(value, limit) {
			return ErrInvalid
		}
	}
	encoded, err := json.Marshal(e)
	if err != nil || len(encoded)+1 > MaximumRecordBytes {
		return ErrInvalid
	}
	return nil
}

func safeIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("_.:-", character)) {
			return false
		}
	}
	return true
}

func cloneEvent(event Event) Event {
	event.Fields = cloneFields(event.Fields)
	return event
}

func cloneFields(fields map[string]string) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	result := make(map[string]string, len(fields))
	for key, value := range fields {
		result[key] = value
	}
	return result
}
