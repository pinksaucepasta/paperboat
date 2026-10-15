// Package connectorprotocol implements the Paperboat connector control-plane
// protocol. It contains only control messages and session state. Byte transport,
// route forwarding, and origin proxying belong to their owning packages.
//
// This shared implementation owns the connector-v1 wire and session contract.
// Database, transport and application adapters live in their owning modules.
package connectorprotocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
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
	if ValidCode(e.Code) {
		return string(e.Code)
	}
	return "connector operation failed"
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

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

func CodeError(base *Error, reason DisconnectReason, retryable bool, cause error) error {
	return &Error{Code: base.Code, Reason: reason, Retryable: retryable, Cause: cause}
}

// protocolError projects only owned finite labels. A fixed traversal budget also
// keeps malformed callback error chains from blocking diagnostic publication.
func protocolError(err error) *Error {
	pending := []error{err}
	for remaining := 16; remaining > 0 && len(pending) > 0; remaining-- {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			continue
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			continue
		}
		if typed, ok := current.(*Error); ok && ValidCode(typed.Code) {
			return typed
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			capacity := remaining - 1 - len(pending)
			if capacity < 0 {
				capacity = 0
			}
			if len(children) > capacity {
				children = children[:capacity]
			}
			pending = append(children, pending...)
		case interface{ Unwrap() error }:
			pending = append([]error{wrapped.Unwrap()}, pending...)
		}
	}
	return nil
}

func CodeOf(err error) Code {
	if typed := protocolError(err); typed != nil {
		return typed.Code
	}
	return ""
}

func ReasonOf(err error) DisconnectReason {
	if typed := protocolError(err); typed != nil && ValidDisconnectReason(typed.Reason) {
		return typed.Reason
	}
	return ""
}

func ValidateIdentifier(value string) error {
	if !identifierPattern.MatchString(value) || len(value) > MaxIdentifierBytes {
		return ErrInvalidInput
	}
	return nil
}

func ValidateCredentialReference(value string) error {
	if len(value) == 0 || len(value) > MaxCredentialRef || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return ErrInvalidInput
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host != "paperboat" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" {
		return ErrInvalidInput
	}
	switch parsed.Scheme {
	case "keychain", "credential-manager", "secret-service", "protected-file", "tpm":
		return nil
	default:
		return ErrInvalidInput
	}
}

func ValidateProof(value string) error {
	if len(value) < 32 || len(value) > MaxProofBytes || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return ErrInvalidInput
	}
	return nil
}

// VerifyProofFunc lets the host identity package keep ownership of public-key
// lookup and revocation while this package owns the signed transcript.
type VerifyProofFunc func(message, signature []byte) bool

func SignProofPayload(payload []byte, sign func([]byte) []byte) (string, error) {
	if len(payload) == 0 || len(payload) > MaxProofBytes || sign == nil {
		return "", ErrInvalidInput
	}
	signature := sign(payload)
	if len(signature) == 0 || len(signature) > MaxProofBytes {
		return "", ErrInvalidInput
	}
	return base64.RawURLEncoding.EncodeToString(signature), nil
}

func VerifyProofPayload(payload []byte, signedProof string, verify VerifyProofFunc) error {
	if len(payload) == 0 || verify == nil {
		return ErrAuthenticationFailed
	}
	signature, err := DecodeProof(signedProof)
	if err != nil || !verify(payload, signature) {
		return ErrAuthenticationFailed
	}
	return nil
}

// DecodeProof decodes the detached base64url signature without exposing any
// credential material. Server adapters use this after deriving the exact
// transcript and resolving the enrolled public identity for the request.
func DecodeProof(signedProof string) ([]byte, error) {
	if ValidateProof(signedProof) != nil {
		return nil, ErrAuthenticationFailed
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(signedProof)
	if err != nil || len(signature) == 0 || len(signature) > MaxProofBytes {
		return nil, ErrAuthenticationFailed
	}
	return signature, nil
}

// ValidateIdentityKey checks the stable public-key identity metadata used by
// the host enrollment store. The private key, certificate bytes, and bearer
// credential never cross this protocol boundary.
func ValidateIdentityKey(keyID, thumbprint string) error {
	if !identityKeyIDPattern.MatchString(keyID) || !identityThumbprintPattern.MatchString(thumbprint) || keyID[len("ed25519:"):] != thumbprint {
		return ErrInvalidInput
	}
	return nil
}

// IdentityThumbprint returns the RFC 7638-style SHA-256 thumbprint for an
// Ed25519 public key. The canonical JSON member order is part of the identity
// contract and matches the host enrollment store.
func IdentityThumbprint(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", ErrInvalidInput
	}
	canonical := `{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(publicKey) + `"}`
	digest := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func IdentityKeyID(publicKey ed25519.PublicKey) (string, error) {
	thumbprint, err := IdentityThumbprint(publicKey)
	if err != nil {
		return "", err
	}
	return "ed25519:" + thumbprint, nil
}

// Capability names are stable wire names. The required set is intentionally
// small so optional data-plane features can be negotiated independently.
const (
	CapabilitySnapshot           = "config.snapshot.v1"
	CapabilityDelta              = "config.delta.v1"
	CapabilityAck                = "config.ack.v1"
	CapabilityHeartbeat          = "session.heartbeat.v1"
	CapabilityRenewal            = "auth.renew.v1"
	CapabilityDrain              = "connector.drain.v1"
	CapabilityCredentialRotation = "credential.rotate.v1"
)

var requiredCapabilities = map[string]struct{}{
	CapabilitySnapshot: {}, CapabilityDelta: {}, CapabilityAck: {},
	CapabilityHeartbeat: {}, CapabilityRenewal: {},
}

// ProductionCapabilities returns a fresh copy of every connector-v1
// capability implemented by the canonical server and stable host runtime.
func ProductionCapabilities() []string {
	return []string{
		CapabilitySnapshot,
		CapabilityDelta,
		CapabilityAck,
		CapabilityHeartbeat,
		CapabilityRenewal,
		CapabilityDrain,
		CapabilityCredentialRotation,
	}
}

func ValidateCapabilities(values []string) error {
	if len(values) == 0 || len(values) > MaxCapabilities {
		return ErrCapabilityMissing
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if len(value) > MaxCapabilityBytes || !capabilityPattern.MatchString(value) {
			return ErrInvalidInput
		}
		if _, exists := seen[value]; exists {
			return ErrInvalidInput
		}
		seen[value] = struct{}{}
	}
	for required := range requiredCapabilities {
		if _, ok := seen[required]; !ok {
			return CodeError(ErrCapabilityMissing, ReasonCapabilityMissing, false, nil)
		}
	}
	return nil
}

func NegotiateCapabilities(offered, supported []string) ([]string, error) {
	if err := ValidateCapabilities(offered); err != nil {
		return nil, err
	}
	if err := ValidateCapabilities(supported); err != nil {
		return nil, err
	}
	supportedSet := make(map[string]struct{}, len(supported))
	for _, value := range supported {
		supportedSet[value] = struct{}{}
	}
	selected := make([]string, 0, len(offered))
	for _, value := range offered {
		if _, ok := supportedSet[value]; ok {
			selected = append(selected, value)
		}
	}
	if err := ValidateCapabilities(selected); err != nil {
		return nil, CodeError(ErrCapabilityMissing, ReasonCapabilityMissing, false, err)
	}
	return selected, nil
}

func NegotiateVersion(minVersion, maxVersion string) (string, error) {
	if !versionPattern.MatchString(minVersion) || !versionPattern.MatchString(maxVersion) {
		return "", CodeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, nil)
	}
	minMajor, minMinor, minOK := parseVersion(minVersion)
	maxMajor, maxMinor, maxOK := parseVersion(maxVersion)
	selectedMajor, selectedMinor, selectedOK := parseVersion(ProtocolVersion)
	if !maxOK || !selectedOK {
		return "", CodeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, nil)
	}
	if !minOK {
		return "", CodeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, nil)
	}
	if minMajor > maxMajor || minMajor > selectedMajor || maxMajor < selectedMajor || minMajor == selectedMajor && minMinor > selectedMinor || maxMajor == selectedMajor && maxMinor < selectedMinor {
		return "", CodeError(ErrProtocolIncompatible, ReasonProtocolMismatch, false, nil)
	}
	return ProtocolVersion, nil
}

func parseVersion(value string) (int, int, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil || major < 0 || minor < 0 || strconv.Itoa(major) != parts[0] || strconv.Itoa(minor) != parts[1] {
		return 0, 0, false
	}
	return major, minor, true
}

type MessageType string

const (
	MessageHello                       MessageType = "hello"
	MessageWelcome                     MessageType = "welcome"
	MessageSnapshot                    MessageType = "snapshot"
	MessageDelta                       MessageType = "delta"
	MessageAck                         MessageType = "ack"
	MessageReady                       MessageType = "ready"
	MessageHeartbeat                   MessageType = "heartbeat"
	MessageHeartbeatAck                MessageType = "heartbeat_ack"
	MessageAuthRenew                   MessageType = "auth_renew"
	MessageAuthRenewed                 MessageType = "auth_renewed"
	MessageDrain                       MessageType = "drain"
	MessageDrainAck                    MessageType = "drain_ack"
	MessageCredentialRotationChallenge MessageType = "credential_rotation_challenge"
	MessageCredentialRotationProof     MessageType = "credential_rotation_proof"
	MessageCredentialRotationInstall   MessageType = "credential_rotation_install"
	MessageCredentialRotationReady     MessageType = "credential_rotation_ready"
	MessageCredentialRotationRevoke    MessageType = "credential_rotation_revoke"
	MessageCredentialRotationAck       MessageType = "credential_rotation_ack"
	MessageDisconnect                  MessageType = "disconnect"
	MessageReject                      MessageType = "reject"
)

const (
	MessageRotationChallenge = MessageCredentialRotationChallenge
	MessageRotationProof     = MessageCredentialRotationProof
	MessageRotationInstall   = MessageCredentialRotationInstall
	MessageRotationReady     = MessageCredentialRotationReady
	MessageRotationRevoke    = MessageCredentialRotationRevoke
	MessageRotationAck       = MessageCredentialRotationAck
)

var allowedMessageTypes = map[MessageType]struct{}{
	MessageHello: {}, MessageWelcome: {}, MessageSnapshot: {}, MessageDelta: {},
	MessageAck: {}, MessageReady: {}, MessageHeartbeat: {}, MessageHeartbeatAck: {},
	MessageAuthRenew: {}, MessageAuthRenewed: {}, MessageDrain: {}, MessageDrainAck: {},
	MessageCredentialRotationChallenge: {}, MessageCredentialRotationProof: {}, MessageCredentialRotationInstall: {}, MessageCredentialRotationReady: {}, MessageCredentialRotationRevoke: {}, MessageCredentialRotationAck: {},
	MessageDisconnect: {}, MessageReject: {},
}

type Frame struct {
	Type      MessageType     `json:"type"`
	Version   string          `json:"version"`
	RequestID string          `json:"request_id"`
	Payload   json.RawMessage `json:"payload"`
}

func NewFrame(messageType MessageType, requestID string, payload any) (Frame, error) {
	if _, ok := allowedMessageTypes[messageType]; !ok || !validRequestID(requestID) {
		return Frame{}, ErrInvalidInput
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Frame{}, CodeError(ErrInvalidInput, ReasonMalformed, false, err)
	}
	if payloadTooLarge(messageType, payload) {
		return Frame{}, ErrFrameTooLarge
	}
	if len(encoded) > maxPayloadBytes(messageType) {
		return Frame{}, ErrFrameTooLarge
	}
	if err := validatePayloadJSON(encoded); err != nil {
		return Frame{}, err
	}
	if err := validateMessagePayload(messageType, encoded); err != nil {
		return Frame{}, err
	}
	return Frame{Type: messageType, Version: ProtocolVersion, RequestID: requestID, Payload: encoded}, nil
}

func payloadTooLarge(messageType MessageType, payload any) bool {
	switch messageType {
	case MessageSnapshot:
		if value, ok := payload.(Snapshot); ok {
			return len(value.Payload) > MaxSnapshotBytes
		}
	case MessageDelta:
		if value, ok := payload.(Delta); ok {
			return len(value.Payload) > MaxDeltaBytes
		}
	}
	return false
}

func (f Frame) Validate() error {
	if _, ok := allowedMessageTypes[f.Type]; !ok {
		return CodeError(ErrUnsupportedMessage, ReasonMalformed, false, nil)
	}
	if f.Version != ProtocolVersion || !validRequestID(f.RequestID) || len(f.Payload) == 0 || len(f.Payload) > maxPayloadBytes(f.Type) {
		return ErrMalformedFrame
	}
	if err := validatePayloadJSON(f.Payload); err != nil {
		return err
	}
	return validateMessagePayload(f.Type, f.Payload)
}

func validateMessagePayload(messageType MessageType, payload []byte) error {
	switch messageType {
	case MessageHello:
		var message Hello
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageWelcome:
		var message Welcome
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageSnapshot:
		var message Snapshot
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.ValidateBound()
	case MessageDelta:
		var message Delta
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.ValidateBound()
	case MessageAck:
		var message Ack
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageReady:
		var message Readiness
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageHeartbeat:
		var message Heartbeat
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageHeartbeatAck:
		var message HeartbeatAck
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageAuthRenew:
		var message RenewalRequest
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageAuthRenewed:
		var message AuthResult
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.ValidateBound()
	case MessageDrain:
		var message Drain
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageDrainAck:
		var message DrainAck
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationChallenge:
		var message CredentialRotationChallenge
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationProof:
		var message CredentialRotationProof
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationInstall:
		var message CredentialRotationInstall
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationReady:
		var message CredentialRotationReady
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationRevoke:
		var message CredentialRotationRevoke
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationAck:
		var message CredentialRotationAck
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageDisconnect:
		var message Disconnect
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageReject:
		var message Reject
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	}
	return ErrUnsupportedMessage
}

func (f Frame) DecodePayload(target any) error {
	if err := f.Validate(); err != nil {
		return err
	}
	return decodeStrict(f.Payload, target)
}

// ClientFrameDecodeFailure identifies a failure produced by decoding an already
// bounded client frame. Its original parser cause remains available locally.
type ClientFrameDecodeFailure struct{ cause error }

func (*ClientFrameDecodeFailure) Error() string   { return "client frame could not be decoded" }
func (e *ClientFrameDecodeFailure) Unwrap() error { return e.cause }

func ReadFrame(reader io.Reader) (Frame, error) {
	if reader == nil {
		return Frame{}, ErrInvalidInput
	}
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return Frame{}, CodeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 {
		return Frame{}, ErrMalformedFrame
	}
	if length > MaxFrameBytes {
		return Frame{}, ErrFrameTooLarge
	}
	body := make([]byte, int(length))
	if _, err := io.ReadFull(reader, body); err != nil {
		return Frame{}, CodeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	if !json.Valid(body) || rejectDuplicateKeys(body) != nil {
		return Frame{}, ErrMalformedFrame
	}
	var frame Frame
	if err := decodeStrict(body, &frame); err != nil {
		return Frame{}, CodeError(ErrMalformedFrame, ReasonMalformed, false, &ClientFrameDecodeFailure{cause: err})
	}
	if err := frame.Validate(); err != nil {
		if _, typed := err.(*Error); typed {
			return Frame{}, err
		}
		return Frame{}, ErrMalformedFrame
	}
	return frame, nil
}

func WriteFrame(writer io.Writer, frame Frame) error {
	if writer == nil {
		return ErrInvalidInput
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return CodeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	if len(body) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	if err := writeAll(writer, prefix[:]); err != nil {
		return err
	}
	return writeAll(writer, body)
}

func maxPayloadBytes(messageType MessageType) int {
	switch messageType {
	case MessageSnapshot:
		return MaxSnapshotBytes + 16<<10
	case MessageDelta:
		return MaxDeltaBytes + 16<<10
	default:
		return 64 << 10
	}
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func validRequestID(value string) bool {
	return len(value) >= 1 && len(value) <= MaxIdentifierBytes && identifierPattern.MatchString(value)
}

func decodeStrict(data []byte, target any) error {
	if len(data) == 0 || len(data) > MaxFrameBytes || target == nil || rejectDuplicateKeys(data) != nil {
		return ErrMalformedFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func validatePayloadJSON(data []byte) error {
	if len(data) == 0 || len(data) > MaxFrameBytes || !json.Valid(data) {
		return ErrMalformedFrame
	}
	if rejectDuplicateKeys(data) != nil {
		return ErrMalformedFrame
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > MaxJSONDepth {
			return errors.New("JSON nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return errors.New("duplicate object key")
				}
				seen[key] = struct{}{}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

// ValidContentHash checks the canonical SHA-256 wire representation.
func ValidContentHash(value string) bool { return hashPattern.MatchString(value) }
