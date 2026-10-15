package connectorprotocol

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

var ErrRotationNotFound = errors.New("credential rotation operation is not available")

// Credential rotation is an aggregate operation. The operation is created for
// one tunnel, but every target connector is tracked independently until the
// replacement session is ready and the old credential generation is revoked.
// No private key, bearer, or reusable enrollment token appears in these
// messages.
const (
	MaxRotationTargets            = 256
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

type RotationPlan struct {
	AccountID     string           `json:"account_id"`
	TunnelID      string           `json:"tunnel_id"`
	OperationID   string           `json:"operation_id"`
	TargetSetHash string           `json:"target_set_hash"`
	Targets       []RotationTarget `json:"targets"`
}

type rotationPlanDocument struct {
	Protocol    string           `json:"protocol"`
	Version     string           `json:"version"`
	AccountID   string           `json:"account_id"`
	TunnelID    string           `json:"tunnel_id"`
	OperationID string           `json:"operation_id"`
	Targets     []RotationTarget `json:"targets"`
}

func NewRotationPlan(accountID, tunnelID, operationID string, targets []RotationTarget) (RotationPlan, error) {
	plan := RotationPlan{AccountID: accountID, TunnelID: tunnelID, OperationID: operationID, Targets: append([]RotationTarget(nil), targets...)}
	sort.Slice(plan.Targets, func(i, j int) bool { return plan.Targets[i].ConnectorID < plan.Targets[j].ConnectorID })
	if err := validateRotationPlanFields(plan); err != nil {
		return RotationPlan{}, err
	}
	digest, err := rotationPlanHash(plan)
	if err != nil {
		return RotationPlan{}, err
	}
	plan.TargetSetHash = digest
	return plan, nil
}

func (p RotationPlan) Validate() error {
	if err := validateRotationPlanFields(p); err != nil {
		return err
	}
	expected, err := rotationPlanHash(p)
	if err != nil {
		return err
	}
	if p.TargetSetHash != expected {
		return CodeError(ErrContentHashMismatch, ReasonSnapshotRejected, false, nil)
	}
	return nil
}

func validateRotationPlanFields(p RotationPlan) error {
	if ValidateIdentifier(p.AccountID) != nil || ValidateIdentifier(p.TunnelID) != nil || ValidateIdentifier(p.OperationID) != nil ||
		!ValidContentHash(p.TargetSetHash) && p.TargetSetHash != "" || len(p.Targets) == 0 || len(p.Targets) > MaxRotationTargets {
		return ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(p.Targets))
	for index, target := range p.Targets {
		if err := target.Validate(); err != nil {
			return err
		}
		if index > 0 && p.Targets[index-1].ConnectorID >= target.ConnectorID {
			return ErrInvalidInput
		}
		if _, ok := seen[target.ConnectorID]; ok {
			return ErrInvalidInput
		}
		seen[target.ConnectorID] = struct{}{}
	}
	return nil
}

func rotationPlanHash(plan RotationPlan) (string, error) {
	payload, err := json.Marshal(rotationPlanDocument{Protocol: ProtocolName, Version: ProtocolVersion, AccountID: plan.AccountID, TunnelID: plan.TunnelID, OperationID: plan.OperationID, Targets: plan.Targets})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (p RotationPlan) Target(connectorID string) (RotationTarget, bool) {
	for _, target := range p.Targets {
		if target.ConnectorID == connectorID {
			return target, true
		}
	}
	return RotationTarget{}, false
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
		c.ProcessGeneration == 0 || !ValidContentHash(c.TargetSetHash) || c.OldCredentialGeneration == 0 || c.NewCredentialGeneration != c.OldCredentialGeneration+1 ||
		ValidateIdentityKey(c.OldIdentityKeyID, c.OldIdentityKeyThumbprint) != nil || !validRotationNonce(c.ChallengeNonce) || c.IssuedAt.IsZero() || c.ExpiresAt.IsZero() || c.OverlapUntil.IsZero() || c.NewCredentialValidUntil.IsZero() || !c.ExpiresAt.After(c.IssuedAt) || !c.OverlapUntil.After(c.ExpiresAt) || !c.NewCredentialValidUntil.After(c.OverlapUntil) || c.ExpiresAt.Sub(c.IssuedAt) > 15*time.Minute || c.OverlapUntil.Sub(c.IssuedAt) > MaxLease || c.NewCredentialValidUntil.Sub(c.IssuedAt) > MaxRotationCredentialLifetime {
		return ErrInvalidInput
	}
	if err := c.Target.Validate(); err != nil || c.Target.ConnectorID != c.ConnectorID || c.Target.HostID != c.HostID || c.Target.OldCredentialGeneration != c.OldCredentialGeneration || c.Target.NewCredentialGeneration != c.NewCredentialGeneration {
		return CodeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
	}
	if !now.IsZero() && (c.IssuedAt.After(now.Add(MaxClockSkew)) || now.Sub(c.IssuedAt) > MaxClockSkew || !c.ExpiresAt.After(now) || !c.OverlapUntil.After(now)) {
		return CodeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
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
		return CodeError(ErrAuthenticationFailed, ReasonAuthentication, false, nil)
	}
	return nil
}

func (p CredentialRotationProof) validate(_ bool) error {
	if ValidateIdentifier(p.AccountID) != nil || ValidateIdentifier(p.TunnelID) != nil || ValidateIdentifier(p.OperationID) != nil || ValidateIdentifier(p.ConnectorID) != nil || ValidateIdentifier(p.HostID) != nil || ValidateIdentifier(p.SessionID) != nil ||
		p.ProcessGeneration == 0 || !ValidContentHash(p.TargetSetHash) || p.OldCredentialGeneration == 0 || p.NewCredentialGeneration != p.OldCredentialGeneration+1 ||
		ValidateIdentityKey(p.OldIdentityKeyID, p.OldIdentityKeyThumbprint) != nil || ValidateIdentityKey(p.NewIdentityKeyID, p.NewIdentityKeyThumbprint) != nil || p.OldIdentityKeyID == p.NewIdentityKeyID || !validRotationNonce(p.ChallengeNonce) || p.IssuedAt.IsZero() || p.NewCredentialValidUntil.IsZero() || ValidateCredentialReference(p.NewCredentialReference) != nil || !p.NewCredentialValidUntil.After(p.IssuedAt) || p.NewCredentialValidUntil.Sub(p.IssuedAt) > MaxRotationCredentialLifetime {
		return ErrInvalidInput
	}
	publicKey, err := DecodeRotationPublicKey(p.NewPublicKey)
	if err != nil {
		return err
	}
	thumbprint, err := IdentityThumbprint(publicKey)
	if err != nil || thumbprint != p.NewIdentityKeyThumbprint {
		return CodeError(ErrIdentityMismatch, ReasonAuthentication, false, nil)
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

type RotationOldProofVerifier func(context.Context, CredentialRotationProof, []byte, []byte) error

// VerifyCredentialRotationProof verifies a rotation proof against the current
// wall clock. Callers that already own a clock (for example deterministic
// tests or a server transaction) should use VerifyCredentialRotationProofAt.
// Keeping the default clocked entry point prevents a public verifier from
// accidentally accepting an old signed proof after a restart.
func VerifyCredentialRotationProof(ctx context.Context, proof CredentialRotationProof, verifyOld RotationOldProofVerifier) error {
	return VerifyCredentialRotationProofAt(ctx, proof, verifyOld, time.Now().UTC())
}

// VerifyCredentialRotationProofAt verifies both signatures and the bounded
// freshness of the proof. The old-key callback remains responsible for the
// durable connector/credential lookup.
func VerifyCredentialRotationProofAt(ctx context.Context, proof CredentialRotationProof, verifyOld RotationOldProofVerifier, now time.Time) error {
	if ctx == nil || verifyOld == nil {
		return ErrAuthenticationFailed
	}
	if err := proof.Validate(now); err != nil {
		return err
	}
	payload, err := CredentialRotationProofPayload(proof)
	if err != nil {
		return err
	}
	oldSignature, err := DecodeProof(proof.OldSignedProof)
	if err != nil {
		return err
	}
	if err := verifyOld(ctx, proof, payload, oldSignature); err != nil {
		return CodeError(ErrAuthenticationFailed, ReasonAuthentication, false, err)
	}
	newSignature, err := DecodeProof(proof.NewSignedProof)
	if err != nil {
		return err
	}
	publicKey, err := DecodeRotationPublicKey(proof.NewPublicKey)
	if err != nil || !ed25519.Verify(publicKey, payload, newSignature) {
		return CodeError(ErrAuthenticationFailed, ReasonAuthentication, false, nil)
	}
	return nil
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
		i.ProcessGeneration == 0 || i.ReplacementProcessGeneration <= i.ProcessGeneration || !ValidContentHash(i.TargetSetHash) || i.OldCredentialGeneration == 0 || i.NewCredentialGeneration != i.OldCredentialGeneration+1 ||
		ValidateIdentityKey(i.NewIdentityKeyID, i.NewIdentityKeyThumbprint) != nil || ValidateCredentialReference(i.NewCredentialReference) != nil || !validRotationNonce(i.ChallengeNonce) || i.OverlapUntil.IsZero() || i.NewCredentialValidUntil.IsZero() || !i.NewCredentialValidUntil.After(i.OverlapUntil) || i.NewCredentialValidUntil.Sub(i.OverlapUntil) > MaxRotationCredentialLifetime {
		return ErrInvalidInput
	}
	publicKey, err := DecodeRotationPublicKey(i.NewPublicKey)
	if err != nil {
		return err
	}
	thumbprint, err := IdentityThumbprint(publicKey)
	if err != nil || thumbprint != i.NewIdentityKeyThumbprint {
		return ErrIdentityMismatch
	}
	if !now.IsZero() && !i.OverlapUntil.After(now) {
		return CodeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
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
		r.ProcessGeneration == 0 || !ValidContentHash(r.TargetSetHash) || r.OldCredentialGeneration == 0 || r.NewCredentialGeneration != r.OldCredentialGeneration+1 || ValidateIdentityKey(r.NewIdentityKeyID, r.NewIdentityKeyThumbprint) != nil || ValidateCredentialReference(r.NewCredentialReference) != nil || r.NewCredentialValidUntil.IsZero() || r.ConfigGeneration == 0 || !ValidContentHash(r.ConfigContentHash) || r.ReadyAt.IsZero() {
		return ErrInvalidInput
	}
	publicKey, err := DecodeRotationPublicKey(r.NewPublicKey)
	if err != nil {
		return err
	}
	thumbprint, err := IdentityThumbprint(publicKey)
	if err != nil || thumbprint != r.NewIdentityKeyThumbprint {
		return ErrIdentityMismatch
	}
	if !r.EdgeReady || !r.RouteReady || !r.OriginReady {
		return CodeError(ErrCredentialRotationNotReady, ReasonSnapshotRejected, true, nil)
	}
	if !now.IsZero() && (!r.NewCredentialValidUntil.After(now) || r.ReadyAt.Before(now.Add(-MaxClockSkew)) || r.ReadyAt.After(now.Add(MaxClockSkew))) {
		if !r.NewCredentialValidUntil.After(now) {
			return CodeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
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
	if ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.OperationID) != nil || ValidateIdentifier(r.ConnectorID) != nil || ValidateIdentifier(r.HostID) != nil || ValidateIdentifier(r.SessionID) != nil || r.ProcessGeneration == 0 || !ValidContentHash(r.TargetSetHash) || r.OldCredentialGeneration == 0 || r.NewCredentialGeneration != r.OldCredentialGeneration+1 || !validRotationNonce(r.RevokeNonce) || r.IssuedAt.IsZero() || r.Deadline.IsZero() || !r.Deadline.After(r.IssuedAt) || r.Deadline.Sub(r.IssuedAt) > DefaultAbortTimeout*2 {
		return ErrInvalidInput
	}
	if !now.IsZero() && (r.IssuedAt.After(now.Add(MaxClockSkew)) || !r.Deadline.After(now)) {
		return CodeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
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
	if ValidateIdentifier(a.AccountID) != nil || ValidateIdentifier(a.TunnelID) != nil || ValidateIdentifier(a.OperationID) != nil || ValidateIdentifier(a.ConnectorID) != nil || ValidateIdentifier(a.HostID) != nil || ValidateIdentifier(a.SessionID) != nil || a.ProcessGeneration == 0 || !ValidContentHash(a.TargetSetHash) || a.OldCredentialGeneration == 0 || a.NewCredentialGeneration != a.OldCredentialGeneration+1 || len(a.Message) > MaxRotationMessage {
		return ErrInvalidInput
	}
	switch a.Status {
	case RotationAckProofAccepted, RotationAckInstalled, RotationAckReady, RotationAckRevoked:
		if a.Code != "" {
			return ErrInvalidInput
		}
	case RotationAckRejected, RotationAckFailed:
		if a.Code == "" || !ValidCode(a.Code) {
			return ErrInvalidInput
		}
	default:
		return ErrInvalidInput
	}
	return nil
}

type RotationTargetState string

const (
	RotationTargetPending    RotationTargetState = "pending"
	RotationTargetChallenged RotationTargetState = "challenged"
	RotationTargetInstalled  RotationTargetState = "installed"
	RotationTargetReady      RotationTargetState = "ready"
	RotationTargetRevoking   RotationTargetState = "revoking"
	RotationTargetRevoked    RotationTargetState = "revoked"
	RotationTargetFailed     RotationTargetState = "failed"
)

func ValidRotationTargetState(state RotationTargetState) bool {
	switch state {
	case RotationTargetPending, RotationTargetChallenged, RotationTargetInstalled, RotationTargetReady, RotationTargetRevoking, RotationTargetRevoked, RotationTargetFailed:
		return true
	default:
		return false
	}
}

type RotationAggregateStatus string

const (
	RotationAggregatePending   RotationAggregateStatus = "pending"
	RotationAggregateSucceeded RotationAggregateStatus = "succeeded"
	RotationAggregateFailed    RotationAggregateStatus = "failed"
)

type RotationTargetSummary struct {
	Target RotationTarget      `json:"target"`
	State  RotationTargetState `json:"state"`
	Code   Code                `json:"code,omitempty"`
}

type RotationSummary struct {
	AccountID     string                  `json:"account_id"`
	TunnelID      string                  `json:"tunnel_id"`
	OperationID   string                  `json:"operation_id"`
	TargetSetHash string                  `json:"target_set_hash"`
	Status        RotationAggregateStatus `json:"status"`
	Targets       []RotationTargetSummary `json:"targets"`
	CompletedAt   time.Time               `json:"completed_at,omitempty"`
}

// RotationPersistence is deliberately narrower than PersistentControlStore.
// Implementations must insert the target set and challenge nonce atomically,
// persist the new verifier generation in overlap state before returning the
// install acknowledgement, bind readiness to the replacement session, and
// revoke the old generation only after every target is ready.
type RotationPersistence interface {
	BeginCredentialRotation(context.Context, RotationPlan) error
	RecordCredentialRotationChallenge(context.Context, CredentialRotationChallenge) error
	RecordCredentialRotationProof(context.Context, CredentialRotationChallenge, CredentialRotationProof, CredentialRotationInstall) error
	RecordCredentialRotationReady(context.Context, CredentialRotationReady) error
	RecordCredentialRotationRevoke(context.Context, CredentialRotationRevoke) error
	RecordCredentialRotationResult(context.Context, CredentialRotationAck, RotationSummary) error
}

// RotationResume is the durable reconstruction boundary. A server restart
// must not turn an already-installed replacement into an orphaned key or
// issue a second challenge for the same immutable target set. The values are
// protocol metadata only: proofs contain signatures and public keys, never
// private or bearer credential bytes.
type RotationResume struct {
	Plan       RotationPlan           `json:"plan"`
	Started    bool                   `json:"started"`
	FinishedAt time.Time              `json:"finished_at,omitempty"`
	Targets    []RotationResumeTarget `json:"targets"`
}

type RotationResumeTarget struct {
	Target                  RotationTarget
	State                   RotationTargetState
	Code                    Code
	OverlapUntil            time.Time
	NewCredentialValidUntil time.Time
	Challenge               CredentialRotationChallenge
	Proof                   CredentialRotationProof
	Install                 CredentialRotationInstall
	Ready                   CredentialRotationReady
	Revoke                  CredentialRotationRevoke
}

// RotationRecoveryStore is optional at the interface level to keep the pure
// in-memory protocol adapter useful in tests. Production SQL stores implement
// it; a caller that needs restart-safe operation handling must call Resume and
// treats an unavailable recovery store as a deployment error.
type RotationRecoveryStore interface {
	LoadCredentialRotation(context.Context, RotationPlan) (RotationResume, error)
}

// RotationSessionAuthorizer is implemented by the production store. It locks
// the authenticated old session and verifies that the negotiated capability
// and credential generation still match before a challenge is emitted.
type RotationSessionAuthorizer interface {
	AuthorizeCredentialRotationSession(context.Context, CredentialRotationChallenge) error
}

type RotationConfig struct {
	Store          RotationPersistence
	VerifyOldProof RotationOldProofVerifier
	Clock          Clock
}

type rotationTargetState struct {
	target    RotationTarget
	state     RotationTargetState
	code      Code
	challenge CredentialRotationChallenge
	proof     CredentialRotationProof
	install   CredentialRotationInstall
	ready     CredentialRotationReady
	revoke    CredentialRotationRevoke
}
