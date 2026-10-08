package connectorprotocol

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Credential rotation is an aggregate operation. The operation is created for
// one tunnel, but every target connector is tracked independently until the
// replacement session is ready and the old credential generation is revoked.
// No private key, bearer, or reusable enrollment token appears in these
// messages.
const (
	MaxRotationMessage            = 512
	MaxRotationCredentialLifetime = 5 * 365 * 24 * time.Hour
)

type RotationTarget struct {
	ConnectorID             string `json:"connector_id"`
	HostID                  string `json:"host_id"`
	OldCredentialGeneration uint64 `json:"old_credential_generation"`
	NewCredentialGeneration uint64 `json:"new_credential_generation"`
}

func (t RotationTarget) Validate() error {
	if ValidateIdentifier(t.ConnectorID) != nil || ValidateIdentifier(t.HostID) != nil ||
		t.OldCredentialGeneration == 0 || t.NewCredentialGeneration != t.OldCredentialGeneration+1 {
		return ErrInvalidInput
	}
	return nil
}

type CredentialRotationChallenge struct {
	AccountID                string         `json:"account_id"`
	TunnelID                 string         `json:"tunnel_id"`
	OperationID              string         `json:"operation_id"`
	ConnectorID              string         `json:"connector_id"`
	HostID                   string         `json:"host_id"`
	SessionID                string         `json:"session_id"`
	ProcessGeneration        uint64         `json:"process_generation"`
	TargetSetHash            string         `json:"target_set_hash"`
	Target                   RotationTarget `json:"target"`
	OldCredentialGeneration  uint64         `json:"old_credential_generation"`
	NewCredentialGeneration  uint64         `json:"new_credential_generation"`
	OldIdentityKeyID         string         `json:"old_identity_key_id"`
	OldIdentityKeyThumbprint string         `json:"old_identity_key_thumbprint"`
	ChallengeNonce           string         `json:"challenge_nonce"`
	IssuedAt                 time.Time      `json:"issued_at"`
	ExpiresAt                time.Time      `json:"expires_at"`
	OverlapUntil             time.Time      `json:"overlap_until"`
	NewCredentialValidUntil  time.Time      `json:"new_credential_valid_until"`
}

func (c CredentialRotationChallenge) Validate(now time.Time) error {
	if ValidateIdentifier(c.AccountID) != nil || ValidateIdentifier(c.TunnelID) != nil || ValidateIdentifier(c.OperationID) != nil || ValidateIdentifier(c.ConnectorID) != nil || ValidateIdentifier(c.HostID) != nil || ValidateIdentifier(c.SessionID) != nil ||
		c.ProcessGeneration == 0 || !hashPattern.MatchString(c.TargetSetHash) || c.OldCredentialGeneration == 0 || c.NewCredentialGeneration != c.OldCredentialGeneration+1 ||
		ValidateIdentityKey(c.OldIdentityKeyID, c.OldIdentityKeyThumbprint) != nil || !validRotationNonce(c.ChallengeNonce) || c.IssuedAt.IsZero() || c.ExpiresAt.IsZero() || c.OverlapUntil.IsZero() || c.NewCredentialValidUntil.IsZero() || !c.ExpiresAt.After(c.IssuedAt) || !c.OverlapUntil.After(c.ExpiresAt) || !c.NewCredentialValidUntil.After(c.OverlapUntil) || c.ExpiresAt.Sub(c.IssuedAt) > 15*time.Minute || c.OverlapUntil.Sub(c.IssuedAt) > MaxLease || c.NewCredentialValidUntil.Sub(c.IssuedAt) > MaxRotationCredentialLifetime {
		return ErrInvalidInput
	}
	if err := c.Target.Validate(); err != nil || c.Target.ConnectorID != c.ConnectorID || c.Target.HostID != c.HostID || c.Target.OldCredentialGeneration != c.OldCredentialGeneration || c.Target.NewCredentialGeneration != c.NewCredentialGeneration {
		return codeError(ErrIdentityMismatch, ReasonAuthentication, false, errors.New("rotation challenge target mismatch"))
	}
	if !now.IsZero() && (c.IssuedAt.After(now.Add(MaxClockSkew)) || now.Sub(c.IssuedAt) > MaxClockSkew || !c.ExpiresAt.After(now) || !c.OverlapUntil.After(now)) {
		return codeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
	}
	return nil
}

type CredentialRotationProof struct {
	AccountID                string    `json:"account_id"`
	TunnelID                 string    `json:"tunnel_id"`
	OperationID              string    `json:"operation_id"`
	ConnectorID              string    `json:"connector_id"`
	HostID                   string    `json:"host_id"`
	SessionID                string    `json:"session_id"`
	ProcessGeneration        uint64    `json:"process_generation"`
	TargetSetHash            string    `json:"target_set_hash"`
	OldCredentialGeneration  uint64    `json:"old_credential_generation"`
	NewCredentialGeneration  uint64    `json:"new_credential_generation"`
	OldIdentityKeyID         string    `json:"old_identity_key_id"`
	OldIdentityKeyThumbprint string    `json:"old_identity_key_thumbprint"`
	NewIdentityKeyID         string    `json:"new_identity_key_id"`
	NewIdentityKeyThumbprint string    `json:"new_identity_key_thumbprint"`
	NewPublicKey             string    `json:"new_public_key"`
	NewCredentialReference   string    `json:"new_credential_reference"`
	ChallengeNonce           string    `json:"challenge_nonce"`
	IssuedAt                 time.Time `json:"issued_at"`
	NewCredentialValidUntil  time.Time `json:"new_credential_valid_until"`
	OldSignedProof           string    `json:"old_signed_proof"`
	NewSignedProof           string    `json:"new_signed_proof"`
}

func (p CredentialRotationProof) Validate(now time.Time) error {
	if err := p.validate(false); err != nil {
		return err
	}
	if ValidateProof(p.OldSignedProof) != nil || ValidateProof(p.NewSignedProof) != nil {
		return ErrInvalidInput
	}
	if !now.IsZero() && (p.IssuedAt.After(now.Add(MaxClockSkew)) || now.Sub(p.IssuedAt) > MaxClockSkew) {
		return codeError(ErrAuthenticationFailed, ReasonAuthentication, false, errors.New("rotation proof is outside clock skew"))
	}
	return nil
}

func (p CredentialRotationProof) validate(_ bool) error {
	if ValidateIdentifier(p.AccountID) != nil || ValidateIdentifier(p.TunnelID) != nil || ValidateIdentifier(p.OperationID) != nil || ValidateIdentifier(p.ConnectorID) != nil || ValidateIdentifier(p.HostID) != nil || ValidateIdentifier(p.SessionID) != nil ||
		p.ProcessGeneration == 0 || !hashPattern.MatchString(p.TargetSetHash) || p.OldCredentialGeneration == 0 || p.NewCredentialGeneration != p.OldCredentialGeneration+1 ||
		ValidateIdentityKey(p.OldIdentityKeyID, p.OldIdentityKeyThumbprint) != nil || ValidateIdentityKey(p.NewIdentityKeyID, p.NewIdentityKeyThumbprint) != nil || p.OldIdentityKeyID == p.NewIdentityKeyID || !validRotationNonce(p.ChallengeNonce) || p.IssuedAt.IsZero() || p.NewCredentialValidUntil.IsZero() || ValidateCredentialReference(p.NewCredentialReference) != nil || !p.NewCredentialValidUntil.After(p.IssuedAt) || p.NewCredentialValidUntil.Sub(p.IssuedAt) > MaxRotationCredentialLifetime {
		return ErrInvalidInput
	}
	publicKey, err := decodeRotationPublicKey(p.NewPublicKey)
	if err != nil {
		return err
	}
	thumbprint, err := IdentityThumbprint(publicKey)
	if err != nil || thumbprint != p.NewIdentityKeyThumbprint {
		return codeError(ErrIdentityMismatch, ReasonAuthentication, false, errors.New("new rotation key thumbprint mismatch"))
	}
	return nil
}

type rotationProofTranscript struct {
	Domain                   string    `json:"domain"`
	Protocol                 string    `json:"protocol"`
	Version                  string    `json:"version"`
	AccountID                string    `json:"account_id"`
	TunnelID                 string    `json:"tunnel_id"`
	OperationID              string    `json:"operation_id"`
	ConnectorID              string    `json:"connector_id"`
	HostID                   string    `json:"host_id"`
	SessionID                string    `json:"session_id"`
	ProcessGeneration        uint64    `json:"process_generation"`
	TargetSetHash            string    `json:"target_set_hash"`
	OldCredentialGeneration  uint64    `json:"old_credential_generation"`
	NewCredentialGeneration  uint64    `json:"new_credential_generation"`
	OldIdentityKeyID         string    `json:"old_identity_key_id"`
	OldIdentityKeyThumbprint string    `json:"old_identity_key_thumbprint"`
	NewIdentityKeyID         string    `json:"new_identity_key_id"`
	NewIdentityKeyThumbprint string    `json:"new_identity_key_thumbprint"`
	NewPublicKey             string    `json:"new_public_key"`
	NewCredentialReference   string    `json:"new_credential_reference"`
	ChallengeNonce           string    `json:"challenge_nonce"`
	IssuedAt                 time.Time `json:"issued_at"`
	NewCredentialValidUntil  time.Time `json:"new_credential_valid_until"`
}

func CredentialRotationProofPayload(proof CredentialRotationProof) ([]byte, error) {
	if err := proof.validate(false); err != nil {
		return nil, err
	}
	return json.Marshal(rotationProofTranscript{
		Domain: "paperboat.connector.credential-rotation.v1", Protocol: ProtocolName, Version: ProtocolVersion,
		AccountID: proof.AccountID, TunnelID: proof.TunnelID, OperationID: proof.OperationID, ConnectorID: proof.ConnectorID, HostID: proof.HostID,
		SessionID: proof.SessionID, ProcessGeneration: proof.ProcessGeneration, TargetSetHash: proof.TargetSetHash,
		OldCredentialGeneration: proof.OldCredentialGeneration, NewCredentialGeneration: proof.NewCredentialGeneration,
		OldIdentityKeyID: proof.OldIdentityKeyID, OldIdentityKeyThumbprint: proof.OldIdentityKeyThumbprint,
		NewIdentityKeyID: proof.NewIdentityKeyID, NewIdentityKeyThumbprint: proof.NewIdentityKeyThumbprint,
		NewPublicKey: proof.NewPublicKey, NewCredentialReference: proof.NewCredentialReference,
		ChallengeNonce: proof.ChallengeNonce, IssuedAt: proof.IssuedAt, NewCredentialValidUntil: proof.NewCredentialValidUntil,
	})
}

func SignCredentialRotationProof(proof CredentialRotationProof, oldSign, newSign func([]byte) []byte) (CredentialRotationProof, error) {
	if oldSign == nil || newSign == nil {
		return CredentialRotationProof{}, ErrInvalidInput
	}
	payload, err := CredentialRotationProofPayload(proof)
	if err != nil {
		return CredentialRotationProof{}, err
	}
	proof.OldSignedProof, err = SignProofPayload(payload, oldSign)
	if err != nil {
		return CredentialRotationProof{}, err
	}
	proof.NewSignedProof, err = SignProofPayload(payload, newSign)
	if err != nil {
		return CredentialRotationProof{}, err
	}
	return proof, proof.Validate(time.Time{})
}

type CredentialRotationInstall struct {
	AccountID                    string    `json:"account_id"`
	TunnelID                     string    `json:"tunnel_id"`
	OperationID                  string    `json:"operation_id"`
	ConnectorID                  string    `json:"connector_id"`
	HostID                       string    `json:"host_id"`
	SessionID                    string    `json:"session_id"`
	ProcessGeneration            uint64    `json:"process_generation"`
	TargetSetHash                string    `json:"target_set_hash"`
	OldCredentialGeneration      uint64    `json:"old_credential_generation"`
	NewCredentialGeneration      uint64    `json:"new_credential_generation"`
	NewIdentityKeyID             string    `json:"new_identity_key_id"`
	NewIdentityKeyThumbprint     string    `json:"new_identity_key_thumbprint"`
	NewPublicKey                 string    `json:"new_public_key"`
	NewCredentialReference       string    `json:"new_credential_reference"`
	ChallengeNonce               string    `json:"challenge_nonce"`
	OverlapUntil                 time.Time `json:"overlap_until"`
	NewCredentialValidUntil      time.Time `json:"new_credential_valid_until"`
	ReplacementProcessGeneration uint64    `json:"replacement_process_generation"`
}

func (i CredentialRotationInstall) Validate(now time.Time) error {
	if ValidateIdentifier(i.AccountID) != nil || ValidateIdentifier(i.TunnelID) != nil || ValidateIdentifier(i.OperationID) != nil || ValidateIdentifier(i.ConnectorID) != nil || ValidateIdentifier(i.HostID) != nil || ValidateIdentifier(i.SessionID) != nil ||
		i.ProcessGeneration == 0 || i.ReplacementProcessGeneration <= i.ProcessGeneration || !hashPattern.MatchString(i.TargetSetHash) || i.OldCredentialGeneration == 0 || i.NewCredentialGeneration != i.OldCredentialGeneration+1 ||
		ValidateIdentityKey(i.NewIdentityKeyID, i.NewIdentityKeyThumbprint) != nil || ValidateCredentialReference(i.NewCredentialReference) != nil || !validRotationNonce(i.ChallengeNonce) || i.OverlapUntil.IsZero() || i.NewCredentialValidUntil.IsZero() || !i.NewCredentialValidUntil.After(i.OverlapUntil) || i.NewCredentialValidUntil.Sub(i.OverlapUntil) > MaxRotationCredentialLifetime {
		return ErrInvalidInput
	}
	publicKey, err := decodeRotationPublicKey(i.NewPublicKey)
	if err != nil {
		return err
	}
	thumbprint, err := IdentityThumbprint(publicKey)
	if err != nil || thumbprint != i.NewIdentityKeyThumbprint {
		return ErrIdentityMismatch
	}
	if !now.IsZero() && !i.OverlapUntil.After(now) {
		return codeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
	}
	return nil
}

type CredentialRotationReady struct {
	AccountID                string    `json:"account_id"`
	TunnelID                 string    `json:"tunnel_id"`
	OperationID              string    `json:"operation_id"`
	ConnectorID              string    `json:"connector_id"`
	HostID                   string    `json:"host_id"`
	SessionID                string    `json:"session_id"`
	PreviousSessionID        string    `json:"previous_session_id"`
	ProcessGeneration        uint64    `json:"process_generation"`
	TargetSetHash            string    `json:"target_set_hash"`
	OldCredentialGeneration  uint64    `json:"old_credential_generation"`
	NewCredentialGeneration  uint64    `json:"new_credential_generation"`
	NewIdentityKeyID         string    `json:"new_identity_key_id"`
	NewIdentityKeyThumbprint string    `json:"new_identity_key_thumbprint"`
	NewPublicKey             string    `json:"new_public_key"`
	NewCredentialReference   string    `json:"new_credential_reference"`
	NewCredentialValidUntil  time.Time `json:"new_credential_valid_until"`
	ConfigGeneration         uint64    `json:"config_generation"`
	ConfigContentHash        string    `json:"config_content_hash"`
	EdgeReady                bool      `json:"edge_ready"`
	RouteReady               bool      `json:"route_ready"`
	OriginReady              bool      `json:"origin_ready"`
	ReadyAt                  time.Time `json:"ready_at"`
}

func (r CredentialRotationReady) Validate(now time.Time) error {
	if ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.OperationID) != nil || ValidateIdentifier(r.ConnectorID) != nil || ValidateIdentifier(r.HostID) != nil || ValidateIdentifier(r.SessionID) != nil || ValidateIdentifier(r.PreviousSessionID) != nil ||
		r.ProcessGeneration == 0 || !hashPattern.MatchString(r.TargetSetHash) || r.OldCredentialGeneration == 0 || r.NewCredentialGeneration != r.OldCredentialGeneration+1 || ValidateIdentityKey(r.NewIdentityKeyID, r.NewIdentityKeyThumbprint) != nil || ValidateCredentialReference(r.NewCredentialReference) != nil || r.NewCredentialValidUntil.IsZero() || r.ConfigGeneration == 0 || !hashPattern.MatchString(r.ConfigContentHash) || r.ReadyAt.IsZero() {
		return ErrInvalidInput
	}
	publicKey, err := decodeRotationPublicKey(r.NewPublicKey)
	if err != nil {
		return err
	}
	thumbprint, err := IdentityThumbprint(publicKey)
	if err != nil || thumbprint != r.NewIdentityKeyThumbprint {
		return ErrIdentityMismatch
	}
	if !r.EdgeReady || !r.RouteReady || !r.OriginReady {
		return codeError(ErrCredentialRotationNotReady, ReasonSnapshotRejected, true, nil)
	}
	if !now.IsZero() && (!r.NewCredentialValidUntil.After(now) || r.ReadyAt.Before(now.Add(-MaxClockSkew)) || r.ReadyAt.After(now.Add(MaxClockSkew))) {
		if !r.NewCredentialValidUntil.After(now) {
			return codeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
		}
		return ErrInvalidInput
	}
	return nil
}

type CredentialRotationRevoke struct {
	AccountID               string    `json:"account_id"`
	TunnelID                string    `json:"tunnel_id"`
	OperationID             string    `json:"operation_id"`
	ConnectorID             string    `json:"connector_id"`
	HostID                  string    `json:"host_id"`
	SessionID               string    `json:"session_id"`
	ProcessGeneration       uint64    `json:"process_generation"`
	TargetSetHash           string    `json:"target_set_hash"`
	OldCredentialGeneration uint64    `json:"old_credential_generation"`
	NewCredentialGeneration uint64    `json:"new_credential_generation"`
	RevokeNonce             string    `json:"revoke_nonce"`
	IssuedAt                time.Time `json:"issued_at"`
	Deadline                time.Time `json:"deadline"`
}

func (r CredentialRotationRevoke) Validate(now time.Time) error {
	if ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.OperationID) != nil || ValidateIdentifier(r.ConnectorID) != nil || ValidateIdentifier(r.HostID) != nil || ValidateIdentifier(r.SessionID) != nil || r.ProcessGeneration == 0 || !hashPattern.MatchString(r.TargetSetHash) || r.OldCredentialGeneration == 0 || r.NewCredentialGeneration != r.OldCredentialGeneration+1 || !validRotationNonce(r.RevokeNonce) || r.IssuedAt.IsZero() || r.Deadline.IsZero() || !r.Deadline.After(r.IssuedAt) || r.Deadline.Sub(r.IssuedAt) > DefaultAbortTimeout*2 {
		return ErrInvalidInput
	}
	if !now.IsZero() && (r.IssuedAt.After(now.Add(MaxClockSkew)) || !r.Deadline.After(now)) {
		return codeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
	}
	return nil
}

type RotationAckStatus string

const (
	RotationAckProofAccepted RotationAckStatus = "proof_accepted"
	RotationAckInstalled     RotationAckStatus = "installed"
	RotationAckReady         RotationAckStatus = "ready"
	RotationAckRevoked       RotationAckStatus = "revoked"
	RotationAckRejected      RotationAckStatus = "rejected"
	RotationAckFailed        RotationAckStatus = "failed"
)

type CredentialRotationAck struct {
	AccountID               string            `json:"account_id"`
	TunnelID                string            `json:"tunnel_id"`
	OperationID             string            `json:"operation_id"`
	ConnectorID             string            `json:"connector_id"`
	HostID                  string            `json:"host_id"`
	SessionID               string            `json:"session_id"`
	ProcessGeneration       uint64            `json:"process_generation"`
	TargetSetHash           string            `json:"target_set_hash"`
	OldCredentialGeneration uint64            `json:"old_credential_generation"`
	NewCredentialGeneration uint64            `json:"new_credential_generation"`
	Status                  RotationAckStatus `json:"status"`
	Code                    Code              `json:"code,omitempty"`
	Message                 string            `json:"message,omitempty"`
}

func (a CredentialRotationAck) Validate() error {
	if ValidateIdentifier(a.AccountID) != nil || ValidateIdentifier(a.TunnelID) != nil || ValidateIdentifier(a.OperationID) != nil || ValidateIdentifier(a.ConnectorID) != nil || ValidateIdentifier(a.HostID) != nil || ValidateIdentifier(a.SessionID) != nil || a.ProcessGeneration == 0 || !hashPattern.MatchString(a.TargetSetHash) || a.OldCredentialGeneration == 0 || a.NewCredentialGeneration != a.OldCredentialGeneration+1 || len(a.Message) > MaxRotationMessage {
		return ErrInvalidInput
	}
	switch a.Status {
	case RotationAckProofAccepted, RotationAckInstalled, RotationAckReady, RotationAckRevoked:
		if a.Code != "" {
			return ErrInvalidInput
		}
	case RotationAckRejected, RotationAckFailed:
		if a.Code == "" || !validCode(a.Code) {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	return nil
}

func validRotationNonce(value string) bool {
	return len(value) >= 16 && len(value) <= MaxNonceBytes && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\r\n")
}

func decodeRotationPublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, ErrInvalidInput
	}
	return ed25519.PublicKey(decoded), nil
}
