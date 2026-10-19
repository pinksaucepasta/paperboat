package connectorprotocol

import (
	"errors"
	"regexp"
	"time"
)

const (
	ProtocolName    = "paperboat.connector"
	ProtocolVersion = "1.0"

	// A frame carries one complete configuration snapshot. The envelope is
	// intentionally larger than MaxSnapshotBytes so the snapshot limit is
	// meaningful on the wire.
	MaxFrameBytes       = MaxSnapshotBytes + 64<<10
	MaxSnapshotBytes    = 4 << 20
	MaxDeltaBytes       = 1 << 20
	MaxCapabilities     = 64
	MaxCapabilityBytes  = 128
	MaxIdentifierBytes  = 128
	MaxCredentialRef    = 512
	MaxProofBytes       = 16 << 10
	MaxNonceBytes       = 128
	MaxReasonMessage    = 512
	MaxJSONDepth        = 64
	MaxClockSkew        = 2 * time.Minute
	DefaultLease        = 45 * time.Second
	DefaultHeartbeat    = 5 * time.Second
	DefaultApplyTimeout = 15 * time.Second
	DefaultAbortTimeout = 5 * time.Second
	MaxLease            = 24 * time.Hour
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

var capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,127}$`)

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

var hashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var identityKeyIDPattern = regexp.MustCompile(`^ed25519:[A-Za-z0-9_-]{43}$`)

var identityThumbprintPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

type Code string

const (
	CodeInvalidInput               Code = "invalid_input"
	CodeMalformedFrame             Code = "malformed_frame"
	CodeFrameTooLarge              Code = "frame_too_large"
	CodeProtocolIncompatible       Code = "protocol_incompatible"
	CodeCapabilityMissing          Code = "capability_missing"
	CodeAuthenticationFailed       Code = "authentication_failed"
	CodeCredentialExpired          Code = "credential_expired"
	CodeIdentityMismatch           Code = "identity_mismatch"
	CodeSessionConflict            Code = "session_conflict"
	CodeSessionClosed              Code = "session_closed"
	CodeLeaseExpired               Code = "lease_expired"
	CodeHeartbeatTimeout           Code = "heartbeat_timeout"
	CodeSnapshotRequired           Code = "snapshot_required"
	CodeGenerationGap              Code = "generation_gap"
	CodeStaleGeneration            Code = "stale_generation"
	CodeContentHashMismatch        Code = "content_hash_mismatch"
	CodeSnapshotRejected           Code = "snapshot_rejected"
	CodeDeltaRejected              Code = "delta_rejected"
	CodeNotReady                   Code = "not_ready"
	CodeCanceled                   Code = "canceled"
	CodeStaleSession               Code = "stale_session"
	CodeUnsupportedMessage         Code = "unsupported_message"
	CodeDrainRejected              Code = "drain_rejected"
	CodeDrainTimeout               Code = "drain_timeout"
	CodeCredentialRotationRejected Code = "credential_rotation_rejected"
	CodeCredentialRotationNotReady Code = "credential_rotation_not_ready"
	CodeCredentialRotationFailed   Code = "credential_rotation_failed"
)

// DisconnectReason is deliberately finite. User-visible diagnostics can map
// these values to recovery text without branching on an error string.
type DisconnectReason string

const (
	ReasonProtocolMismatch   DisconnectReason = "protocol_mismatch"
	ReasonCapabilityMissing  DisconnectReason = "capability_missing"
	ReasonMalformed          DisconnectReason = "malformed_message"
	ReasonAuthentication     DisconnectReason = "authentication_failed"
	ReasonCredentialExpired  DisconnectReason = "credential_expired"
	ReasonLeaseExpired       DisconnectReason = "lease_expired"
	ReasonHeartbeatTimeout   DisconnectReason = "heartbeat_timeout"
	ReasonSessionReplaced    DisconnectReason = "session_replaced"
	ReasonStaleGeneration    DisconnectReason = "stale_generation"
	ReasonSnapshotRejected   DisconnectReason = "snapshot_rejected"
	ReasonGenerationGap      DisconnectReason = "generation_gap"
	ReasonCredentialRotation DisconnectReason = "credential_rotation"
	ReasonCanceled           DisconnectReason = "canceled"
	ReasonServerShutdown     DisconnectReason = "server_shutdown"
	ReasonProtocolClosed     DisconnectReason = "protocol_closed"
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
	ErrInvalidInput               = &Error{Code: CodeInvalidInput}
	ErrMalformedFrame             = &Error{Code: CodeMalformedFrame}
	ErrFrameTooLarge              = &Error{Code: CodeFrameTooLarge}
	ErrProtocolIncompatible       = &Error{Code: CodeProtocolIncompatible}
	ErrCapabilityMissing          = &Error{Code: CodeCapabilityMissing}
	ErrAuthenticationFailed       = &Error{Code: CodeAuthenticationFailed}
	ErrCredentialExpired          = &Error{Code: CodeCredentialExpired}
	ErrIdentityMismatch           = &Error{Code: CodeIdentityMismatch}
	ErrSessionConflict            = &Error{Code: CodeSessionConflict}
	ErrSessionClosed              = &Error{Code: CodeSessionClosed}
	ErrLeaseExpired               = &Error{Code: CodeLeaseExpired}
	ErrHeartbeatTimeout           = &Error{Code: CodeHeartbeatTimeout}
	ErrSnapshotRequired           = &Error{Code: CodeSnapshotRequired}
	ErrGenerationGap              = &Error{Code: CodeGenerationGap}
	ErrStaleGeneration            = &Error{Code: CodeStaleGeneration}
	ErrContentHashMismatch        = &Error{Code: CodeContentHashMismatch}
	ErrSnapshotRejected           = &Error{Code: CodeSnapshotRejected}
	ErrDeltaRejected              = &Error{Code: CodeDeltaRejected}
	ErrNotReady                   = &Error{Code: CodeNotReady}
	ErrCanceled                   = &Error{Code: CodeCanceled}
	ErrStaleSession               = &Error{Code: CodeStaleSession}
	ErrUnsupportedMessage         = &Error{Code: CodeUnsupportedMessage}
	ErrDrainRejected              = &Error{Code: CodeDrainRejected}
	ErrDrainTimeout               = &Error{Code: CodeDrainTimeout}
	ErrCredentialRotationRejected = &Error{Code: CodeCredentialRotationRejected}
	ErrCredentialRotationNotReady = &Error{Code: CodeCredentialRotationNotReady}
	ErrCredentialRotationFailed   = &Error{Code: CodeCredentialRotationFailed}
)

func codeError(base *Error, reason DisconnectReason, retryable bool, cause error) error {
	return &Error{Code: base.Code, Reason: reason, Retryable: retryable, Cause: cause}
}

func CodeOf(err error) Code {
	var typed *Error
	if errors.As(err, &typed) && typed != nil {
		return typed.Code
	}
	return ""
}

func ReasonOf(err error) DisconnectReason {
	var typed *Error
	if errors.As(err, &typed) && typed != nil {
		return typed.Reason
	}
	return ""
}
