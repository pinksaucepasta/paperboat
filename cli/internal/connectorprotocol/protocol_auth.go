package connectorprotocol

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type AuthRequest struct {
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	HostID                string    `json:"host_id"`
	IdentityKeyID         string    `json:"identity_key_id"`
	IdentityKeyThumbprint string    `json:"identity_key_thumbprint"`
	ProcessGeneration     uint64    `json:"process_generation"`
	CredentialGeneration  uint64    `json:"credential_generation"`
	Nonce                 string    `json:"nonce"`
	SignedProof           string    `json:"signed_proof"`
	IssuedAt              time.Time `json:"issued_at"`
	ExpiresAt             time.Time `json:"expires_at"`
}

type authProofTranscript struct {
	Domain                string    `json:"domain"`
	Protocol              string    `json:"protocol"`
	Version               string    `json:"version"`
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	HostID                string    `json:"host_id"`
	IdentityKeyID         string    `json:"identity_key_id"`
	IdentityKeyThumbprint string    `json:"identity_key_thumbprint"`
	ProcessGeneration     uint64    `json:"process_generation"`
	CredentialGeneration  uint64    `json:"credential_generation"`
	SessionID             string    `json:"session_id,omitempty"`
	Nonce                 string    `json:"nonce"`
	IssuedAt              time.Time `json:"issued_at,omitempty"`
	ExpiresAt             time.Time `json:"expires_at,omitempty"`
	RequestedAt           time.Time `json:"requested_at,omitempty"`
}

func AuthProofPayload(request AuthRequest) ([]byte, error) {
	if err := request.validate(time.Time{}, false); err != nil {
		return nil, err
	}
	return json.Marshal(authProofTranscript{
		Domain: "paperboat.connector.auth.v1", Protocol: ProtocolName, Version: ProtocolVersion,
		AccountID: request.AccountID, TunnelID: request.TunnelID, ConnectorID: request.ConnectorID,
		HostID: request.HostID, IdentityKeyID: request.IdentityKeyID, IdentityKeyThumbprint: request.IdentityKeyThumbprint,
		ProcessGeneration: request.ProcessGeneration, CredentialGeneration: request.CredentialGeneration,
		Nonce: request.Nonce, IssuedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt,
	})
}

func RenewalProofPayload(request RenewalRequest) ([]byte, error) {
	if err := request.validate(false); err != nil {
		return nil, err
	}
	return json.Marshal(authProofTranscript{
		Domain: "paperboat.connector.auth.renew.v1", Protocol: ProtocolName, Version: ProtocolVersion,
		AccountID: request.AccountID, TunnelID: request.TunnelID, ConnectorID: request.ConnectorID,
		HostID: request.HostID, IdentityKeyID: request.IdentityKeyID, IdentityKeyThumbprint: request.IdentityKeyThumbprint,
		ProcessGeneration: request.ProcessGeneration, CredentialGeneration: request.CredentialGeneration,
		SessionID: request.SessionID, Nonce: request.Nonce, RequestedAt: request.RequestedAt,
	})
}

func VerifyAuthProof(request AuthRequest, verify VerifyProofFunc) error {
	payload, err := AuthProofPayload(request)
	if err != nil {
		return err
	}
	return VerifyProofPayload(payload, request.SignedProof, verify)
}

func SignAuthProof(request AuthRequest, sign func([]byte) []byte) (AuthRequest, error) {
	payload, err := AuthProofPayload(request)
	if err != nil {
		return AuthRequest{}, err
	}
	request.SignedProof, err = SignProofPayload(payload, sign)
	if err != nil {
		return AuthRequest{}, err
	}
	return request, request.Validate(time.Time{})
}

func VerifyRenewalProof(request RenewalRequest, verify VerifyProofFunc) error {
	payload, err := RenewalProofPayload(request)
	if err != nil {
		return err
	}
	return VerifyProofPayload(payload, request.SignedProof, verify)
}

func SignRenewalProof(request RenewalRequest, sign func([]byte) []byte) (RenewalRequest, error) {
	payload, err := RenewalProofPayload(request)
	if err != nil {
		return RenewalRequest{}, err
	}
	request.SignedProof, err = SignProofPayload(payload, sign)
	if err != nil {
		return RenewalRequest{}, err
	}
	return request, request.Validate()
}

func (a AuthRequest) Validate(now time.Time) error {
	return a.validate(now, true)
}

func (a AuthRequest) validate(now time.Time, requireProof bool) error {
	if ValidateIdentifier(a.AccountID) != nil || ValidateIdentifier(a.TunnelID) != nil || ValidateIdentifier(a.ConnectorID) != nil || ValidateIdentifier(a.HostID) != nil || ValidateIdentityKey(a.IdentityKeyID, a.IdentityKeyThumbprint) != nil {
		return ErrInvalidInput
	}
	if len(a.Nonce) < 16 || len(a.Nonce) > MaxNonceBytes || strings.TrimSpace(a.Nonce) != a.Nonce || strings.ContainsAny(a.Nonce, "\r\n") || a.ProcessGeneration == 0 || a.CredentialGeneration == 0 || a.IssuedAt.IsZero() || a.ExpiresAt.IsZero() || !a.ExpiresAt.After(a.IssuedAt) || a.ExpiresAt.Sub(a.IssuedAt) > 15*time.Minute || requireProof && ValidateProof(a.SignedProof) != nil {
		return ErrInvalidInput
	}
	if !now.IsZero() && (a.IssuedAt.After(now.Add(MaxClockSkew)) || now.Sub(a.IssuedAt) > MaxClockSkew || !a.ExpiresAt.After(now)) {
		return codeError(ErrCredentialExpired, ReasonCredentialExpired, true, nil)
	}
	return nil
}

type AuthResult struct {
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	SessionID             string    `json:"session_id,omitempty"`
	HostID                string    `json:"host_id"`
	IdentityKeyID         string    `json:"identity_key_id"`
	IdentityKeyThumbprint string    `json:"identity_key_thumbprint"`
	ProcessGeneration     uint64    `json:"process_generation"`
	CredentialGeneration  uint64    `json:"credential_generation"`
	CredentialExpiresAt   time.Time `json:"credential_expires_at"`
	LeaseExpiresAt        time.Time `json:"lease_expires_at"`
}

func (a AuthResult) Validate(now time.Time) error {
	for _, value := range []string{a.AccountID, a.TunnelID, a.ConnectorID, a.HostID} {
		if ValidateIdentifier(value) != nil {
			return ErrAuthenticationFailed
		}
	}
	if a.SessionID != "" && ValidateIdentifier(a.SessionID) != nil {
		return ErrAuthenticationFailed
	}
	if ValidateIdentityKey(a.IdentityKeyID, a.IdentityKeyThumbprint) != nil || a.ProcessGeneration == 0 || a.CredentialGeneration == 0 || a.CredentialExpiresAt.IsZero() || a.LeaseExpiresAt.IsZero() || a.LeaseExpiresAt.After(a.CredentialExpiresAt) {
		return ErrAuthenticationFailed
	}
	if !now.IsZero() && (!a.CredentialExpiresAt.After(now) || !a.LeaseExpiresAt.After(now) || a.LeaseExpiresAt.Sub(now) > MaxLease) {
		return ErrAuthenticationFailed
	}
	return nil
}

func (a AuthResult) ValidateBound() error {
	if err := a.Validate(time.Time{}); err != nil || ValidateIdentifier(a.SessionID) != nil {
		return ErrAuthenticationFailed
	}
	return nil
}

type RenewalRequest struct {
	SessionID             string    `json:"session_id"`
	AccountID             string    `json:"account_id"`
	TunnelID              string    `json:"tunnel_id"`
	ConnectorID           string    `json:"connector_id"`
	HostID                string    `json:"host_id"`
	IdentityKeyID         string    `json:"identity_key_id"`
	IdentityKeyThumbprint string    `json:"identity_key_thumbprint"`
	ProcessGeneration     uint64    `json:"process_generation"`
	CredentialGeneration  uint64    `json:"credential_generation"`
	Nonce                 string    `json:"nonce"`
	SignedProof           string    `json:"signed_proof"`
	RequestedAt           time.Time `json:"requested_at"`
}

func (r RenewalRequest) Validate() error {
	return r.validate(true)
}

// ValidateAt adds freshness checks that cannot be encoded in the detached
// signature alone. Durable server adapters must call this before accepting a
// renewal so an old signed nonce cannot be replayed after a process restart.
func (r RenewalRequest) ValidateAt(now time.Time) error {
	if err := r.validate(true); err != nil {
		return err
	}
	if !now.IsZero() && (r.RequestedAt.After(now.Add(MaxClockSkew)) || now.Sub(r.RequestedAt) > MaxClockSkew) {
		return codeError(ErrAuthenticationFailed, ReasonAuthentication, false, errors.New("renewal request is outside clock skew"))
	}
	return nil
}

func (r RenewalRequest) validate(requireProof bool) error {
	if ValidateIdentifier(r.SessionID) != nil || ValidateIdentifier(r.AccountID) != nil || ValidateIdentifier(r.TunnelID) != nil || ValidateIdentifier(r.ConnectorID) != nil || ValidateIdentifier(r.HostID) != nil || ValidateIdentityKey(r.IdentityKeyID, r.IdentityKeyThumbprint) != nil || r.ProcessGeneration == 0 || r.CredentialGeneration == 0 || len(r.Nonce) < 16 || len(r.Nonce) > MaxNonceBytes || strings.TrimSpace(r.Nonce) != r.Nonce || strings.ContainsAny(r.Nonce, "\r\n") || r.RequestedAt.IsZero() || requireProof && ValidateProof(r.SignedProof) != nil {
		return ErrInvalidInput
	}
	return nil
}

type Lease struct {
	SessionID           string    `json:"session_id"`
	ExpiresAt           time.Time `json:"expires_at"`
	HeartbeatIntervalMS uint32    `json:"heartbeat_interval_ms"`
}

func (l Lease) Validate(now time.Time) error {
	if ValidateIdentifier(l.SessionID) != nil || l.ExpiresAt.IsZero() || l.HeartbeatIntervalMS == 0 {
		return ErrInvalidInput
	}
	if !now.IsZero() && (!l.ExpiresAt.After(now) || l.ExpiresAt.Sub(now) > MaxLease || time.Duration(l.HeartbeatIntervalMS)*time.Millisecond >= l.ExpiresAt.Sub(now)) {
		return ErrInvalidInput
	}
	return nil
}
