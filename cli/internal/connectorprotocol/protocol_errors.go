package connectorprotocol

import (
	"regexp"
	"time"
)

const (
	ProtocolName    = "paperboat.connector"
	ProtocolVersion = "1.0"

	MaxFrameBytes      = 4<<20 + 64<<10
	MaxIdentifierBytes = 128
	MaxJSONDepth       = 64
	MaxClockSkew       = 2 * time.Minute
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type Code string

const (
	CodeInvalidInput    Code = "invalid_input"
	CodeMalformedFrame  Code = "malformed_frame"
	CodeFrameTooLarge   Code = "frame_too_large"
	CodeSessionConflict Code = "session_conflict"
	CodeLeaseExpired    Code = "lease_expired"
	CodeStaleGeneration Code = "stale_generation"
	CodeNotReady        Code = "not_ready"
	CodeCanceled        Code = "canceled"
)

// DisconnectReason is deliberately finite. User-visible diagnostics can map
// these values to recovery text without branching on an error string.
type DisconnectReason string

const (
	ReasonMalformed DisconnectReason = "malformed_message"
)

// Error is the typed boundary error returned by this package. Retryable is a
// protocol decision, not a claim that the caller may repeat an unsafe write.
type Error struct {
	Code       Code
	Reason     DisconnectReason
	Retryable  bool
	RetryAfter time.Duration
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause == nil {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Cause.Error()
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other != nil && e != nil && e.Code == other.Code
}

func (e *Error) Temporary() bool { return e != nil && e.Retryable }

var (
	ErrInvalidInput    = &Error{Code: CodeInvalidInput}
	ErrMalformedFrame  = &Error{Code: CodeMalformedFrame}
	ErrFrameTooLarge   = &Error{Code: CodeFrameTooLarge}
	ErrSessionConflict = &Error{Code: CodeSessionConflict}
	ErrLeaseExpired    = &Error{Code: CodeLeaseExpired}
	ErrStaleGeneration = &Error{Code: CodeStaleGeneration}
	ErrNotReady        = &Error{Code: CodeNotReady}
	ErrCanceled        = &Error{Code: CodeCanceled}
)

func codeError(base *Error, reason DisconnectReason, retryable bool, cause error) error {
	return &Error{Code: base.Code, Reason: reason, Retryable: retryable, Cause: cause}
}
