package connectorprotocol

import (
	"time"
)

type AckKind string

const (
	AckSnapshot AckKind = "snapshot"
	AckDelta    AckKind = "delta"
	AckReady    AckKind = "ready"
	AckRenewal  AckKind = "auth_renew"
)

type AckStatus string

const (
	AckApplied          AckStatus = "applied"
	AckDuplicate        AckStatus = "duplicate"
	AckRejected         AckStatus = "rejected"
	AckSnapshotRequired AckStatus = "snapshot_required"
)

type Ack struct {
	AccountID         string    `json:"account_id"`
	TunnelID          string    `json:"tunnel_id"`
	ConnectorID       string    `json:"connector_id"`
	SessionID         string    `json:"session_id"`
	ProcessGeneration uint64    `json:"process_generation"`
	Kind              AckKind   `json:"kind"`
	Status            AckStatus `json:"status"`
	Generation        uint64    `json:"generation"`
	ContentHash       string    `json:"content_hash"`
	Code              Code      `json:"code,omitempty"`
}

func (a Ack) Validate() error {
	if ValidateIdentifier(a.AccountID) != nil || ValidateIdentifier(a.TunnelID) != nil || ValidateIdentifier(a.ConnectorID) != nil || ValidateIdentifier(a.SessionID) != nil || a.ProcessGeneration == 0 || (a.Kind != AckSnapshot && a.Kind != AckDelta && a.Kind != AckReady && a.Kind != AckRenewal) || (a.Status != AckApplied && a.Status != AckDuplicate && a.Status != AckRejected && a.Status != AckSnapshotRequired) || a.Generation == 0 || !hashPattern.MatchString(a.ContentHash) {
		return ErrInvalidInput
	}
	if a.Status == AckRejected && (a.Code == "" || !validCode(a.Code)) || a.Status != AckRejected && a.Code != "" {
		return ErrInvalidInput
	}
	return nil
}

type Readiness struct {
	AccountID         string `json:"account_id"`
	SessionID         string `json:"session_id"`
	TunnelID          string `json:"tunnel_id"`
	ConnectorID       string `json:"connector_id"`
	ProcessGeneration uint64 `json:"process_generation"`
	Generation        uint64 `json:"generation"`
	ContentHash       string `json:"content_hash"`
	EdgeReady         bool   `json:"edge_ready"`
	RouteReady        bool   `json:"route_ready"`
	OriginReady       bool   `json:"origin_ready"`
}

func (r Readiness) Validate() error {
	if ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.SessionID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.ConnectorID) != nil || r.ProcessGeneration == 0 || r.Generation == 0 || !hashPattern.MatchString(r.ContentHash) {
		return ErrInvalidInput
	}
	return nil
}

type Heartbeat struct {
	AccountID             string    `json:"account_id"`
	SessionID             string    `json:"session_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	ProcessGeneration     uint64    `json:"process_generation"`
	LastAppliedGeneration uint64    `json:"last_applied_generation"`
	LastAppliedHash       string    `json:"last_applied_content_hash"`
	SentAt                time.Time `json:"sent_at"`
}

func (h Heartbeat) Validate() error {
	if ValidateIdentifier(h.AccountID) != nil || ValidateIdentifier(h.SessionID) != nil || ValidateIdentifier(h.TunnelID) != nil || ValidateIdentifier(h.ConnectorID) != nil || h.ProcessGeneration == 0 || h.LastAppliedGeneration == 0 || !hashPattern.MatchString(h.LastAppliedHash) || h.SentAt.IsZero() {
		return ErrInvalidInput
	}
	return nil
}

type HeartbeatAck struct {
	AccountID         string    `json:"account_id"`
	TunnelID          string    `json:"tunnel_id"`
	ConnectorID       string    `json:"connector_id"`
	SessionID         string    `json:"session_id"`
	ProcessGeneration uint64    `json:"process_generation"`
	LeaseExpiresAt    time.Time `json:"lease_expires_at"`
	ServerTime        time.Time `json:"server_time"`
}

func (h HeartbeatAck) Validate(now time.Time) error {
	if ValidateIdentifier(h.AccountID) != nil || ValidateIdentifier(h.TunnelID) != nil || ValidateIdentifier(h.ConnectorID) != nil || ValidateIdentifier(h.SessionID) != nil || h.ProcessGeneration == 0 || h.LeaseExpiresAt.IsZero() || !h.LeaseExpiresAt.After(now) || h.ServerTime.IsZero() {
		return ErrInvalidInput
	}
	return nil
}

// DrainStatus is the finite state reported by a connector while it stops
// admitting new streams and lets existing streams finish.
type DrainStatus string

const (
	DrainAccepted  DrainStatus = "accepted"
	DrainProgress  DrainStatus = "progress"
	DrainCompleted DrainStatus = "completed"
	DrainForced    DrainStatus = "forced_close"
	DrainRejected  DrainStatus = "rejected"
)

const MaxActiveStreams = 1 << 20

// Drain is a server-to-connector request. The target generation and hash are
// part of the request so a stale process cannot drain a replacement session or
// a different configuration. StopNewStreams is deliberately required: a
// request that only looks like a status probe is not a drain operation.
type Drain struct {
	AccountID          string    `json:"account_id"`
	TunnelID           string    `json:"tunnel_id"`
	ConnectorID        string    `json:"connector_id"`
	SessionID          string    `json:"session_id"`
	ProcessGeneration  uint64    `json:"process_generation"`
	DrainID            string    `json:"drain_id"`
	Generation         uint64    `json:"generation"`
	ContentHash        string    `json:"content_hash"`
	Deadline           time.Time `json:"deadline"`
	StopNewStreams     bool      `json:"stop_new_streams"`
	ForceAfterDeadline bool      `json:"force_after_deadline"`
}

func (d Drain) Validate(now time.Time) error {
	if ValidateIdentifier(d.AccountID) != nil || ValidateIdentifier(d.TunnelID) != nil || ValidateIdentifier(d.ConnectorID) != nil || ValidateIdentifier(d.SessionID) != nil || ValidateIdentifier(d.DrainID) != nil || d.ProcessGeneration == 0 || d.Generation == 0 || !hashPattern.MatchString(d.ContentHash) || d.Deadline.IsZero() || !d.StopNewStreams {
		return ErrInvalidInput
	}
	if !now.IsZero() && (!d.Deadline.After(now) || d.Deadline.Sub(now) > MaxLease) {
		return ErrInvalidInput
	}
	return nil
}

// DrainAck is emitted first when the connector accepts the drain and again as
// streams leave. Completion is explicit. A forced close is represented by a
// distinct status and code, so callers do not have to infer it from a message.
type DrainAck struct {
	AccountID         string      `json:"account_id"`
	TunnelID          string      `json:"tunnel_id"`
	ConnectorID       string      `json:"connector_id"`
	SessionID         string      `json:"session_id"`
	ProcessGeneration uint64      `json:"process_generation"`
	DrainID           string      `json:"drain_id"`
	Generation        uint64      `json:"generation"`
	ContentHash       string      `json:"content_hash"`
	Status            DrainStatus `json:"status"`
	ActiveStreams     uint32      `json:"active_streams"`
	ForcedClose       bool        `json:"forced_close"`
	Code              Code        `json:"code,omitempty"`
}

func (a DrainAck) Validate(now time.Time) error {
	if ValidateIdentifier(a.AccountID) != nil || ValidateIdentifier(a.TunnelID) != nil || ValidateIdentifier(a.ConnectorID) != nil || ValidateIdentifier(a.SessionID) != nil || ValidateIdentifier(a.DrainID) != nil || a.ProcessGeneration == 0 || a.Generation == 0 || !hashPattern.MatchString(a.ContentHash) || a.ActiveStreams > MaxActiveStreams {
		return ErrInvalidInput
	}
	switch a.Status {
	case DrainAccepted, DrainProgress:
		if a.ForcedClose || a.Code != "" {
			return ErrInvalidInput
		}
	case DrainCompleted:
		if a.ActiveStreams != 0 || a.ForcedClose || a.Code != "" {
			return ErrInvalidInput
		}
	case DrainForced:
		if !a.ForcedClose || a.Code != CodeDrainTimeout || a.ActiveStreams != 0 {
			return ErrInvalidInput
		}
	case DrainRejected:
		if a.ForcedClose || a.Code != CodeDrainRejected {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	if a.Code != "" && !validCode(a.Code) {
		return ErrInvalidInput
	}
	_ = now
	return nil
}

type Disconnect struct {
	AccountID         string           `json:"account_id"`
	TunnelID          string           `json:"tunnel_id"`
	ConnectorID       string           `json:"connector_id"`
	SessionID         string           `json:"session_id"`
	ProcessGeneration uint64           `json:"process_generation"`
	Reason            DisconnectReason `json:"reason"`
	Retryable         bool             `json:"retryable"`
	Message           string           `json:"message,omitempty"`
}

type Reject struct {
	AccountID         string           `json:"account_id"`
	TunnelID          string           `json:"tunnel_id"`
	ConnectorID       string           `json:"connector_id"`
	SessionID         string           `json:"session_id"`
	ProcessGeneration uint64           `json:"process_generation"`
	Code              Code             `json:"code"`
	Reason            DisconnectReason `json:"reason"`
	Retryable         bool             `json:"retryable"`
	Message           string           `json:"message,omitempty"`
}

func (r Reject) Validate() error {
	if ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.ConnectorID) != nil || ValidateIdentifier(r.SessionID) != nil || r.ProcessGeneration == 0 || !validCode(r.Code) || !validDisconnectReason(r.Reason) || len(r.Message) > MaxReasonMessage {
		return ErrInvalidInput
	}
	return nil
}

func (d Disconnect) Validate() error {
	if ValidateIdentifier(d.AccountID) != nil || ValidateIdentifier(d.TunnelID) != nil || ValidateIdentifier(d.ConnectorID) != nil || ValidateIdentifier(d.SessionID) != nil || d.ProcessGeneration == 0 || !validDisconnectReason(d.Reason) || len(d.Message) > MaxReasonMessage {
		return ErrInvalidInput
	}
	return nil
}

func validDisconnectReason(reason DisconnectReason) bool {
	switch reason {
	case ReasonProtocolMismatch, ReasonCapabilityMissing, ReasonMalformed, ReasonAuthentication, ReasonCredentialExpired, ReasonLeaseExpired, ReasonHeartbeatTimeout, ReasonSessionReplaced, ReasonStaleGeneration, ReasonSnapshotRejected, ReasonGenerationGap, ReasonCredentialRotation, ReasonCanceled, ReasonServerShutdown, ReasonProtocolClosed:
		return true
	default:
		return false
	}
}

func validCode(code Code) bool {
	switch code {
	case CodeInvalidInput, CodeMalformedFrame, CodeFrameTooLarge, CodeProtocolIncompatible, CodeCapabilityMissing, CodeAuthenticationFailed, CodeCredentialExpired, CodeIdentityMismatch, CodeSessionConflict, CodeSessionClosed, CodeLeaseExpired, CodeHeartbeatTimeout, CodeSnapshotRequired, CodeGenerationGap, CodeStaleGeneration, CodeContentHashMismatch, CodeSnapshotRejected, CodeDeltaRejected, CodeNotReady, CodeCanceled, CodeStaleSession, CodeUnsupportedMessage, CodeDrainRejected, CodeDrainTimeout, CodeCredentialRotationRejected, CodeCredentialRotationNotReady, CodeCredentialRotationFailed:
		return true
	default:
		return false
	}
}
