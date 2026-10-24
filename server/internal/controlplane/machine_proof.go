package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

type MachineRequestClaims struct {
	MachineID              string
	EnvironmentID          string
	UserID                 string
	InstallationGeneration int64
	OperationID            string
	CredentialJTI          string
	SessionGeneration      int64
}

type canonicalMachineProofClaims struct {
	MachineID              string    `json:"machine_id"`
	EnvironmentID          string    `json:"environment_id"`
	InstallationGeneration int64     `json:"installation_generation"`
	OperationID            string    `json:"operation_id"`
	Method                 string    `json:"method"`
	Path                   string    `json:"path"`
	BodySHA256             string    `json:"body_sha256"`
	IssuedAt               time.Time `json:"issued_at"`
	ExpiresAt              time.Time `json:"expires_at"`
	Nonce                  string    `json:"nonce,omitempty"`
}

func (s *EnrollmentService) VerifyMachineRequest(ctx context.Context, identityToken string, proof []byte, method, path string, body []byte) (MachineRequestClaims, error) {
	claims, err := s.VerifyMachineControlRequest(ctx, identityToken, proof, method, path, body)
	if err == nil {
		return claims, nil
	}
	// A helper proof is a supported credential fallback only when the machine
	// credential itself was rejected. Database and other operational failures
	// from machine-control verification must remain visible to the API owner.
	// A clock outside the window fails a helper proof the same way; keep the reason.
	if !errors.Is(err, ErrHelperProof) || errors.Is(err, ErrClockSkew) {
		return MachineRequestClaims{}, err
	}
	controlReason := ProofRejectionReason(err)
	helper, err := s.VerifyHelperRequest(ctx, identityToken, proof, method, path, body)
	if err != nil {
		if errors.Is(err, ErrHelperProof) && !errors.Is(err, ErrClockSkew) {
			// Both credential paths refused it: keep the machine-control reason, the likely one.
			return MachineRequestClaims{}, proofRejected(controlReason + "; helper " + ProofRejectionReason(err))
		}
		return MachineRequestClaims{}, err
	}
	machine, err := s.store.Queries().GetActiveUserMachineForControl(ctx, helper.MachineID)
	if err != nil {
		return MachineRequestClaims{}, machineProofLookupError(err)
	}
	if machine.EnvironmentID != helper.EnvironmentID {
		return MachineRequestClaims{}, ErrHelperProof
	}
	return MachineRequestClaims{MachineID: machine.ID, EnvironmentID: machine.EnvironmentID, UserID: machine.UserID, InstallationGeneration: machine.InstallationGeneration, OperationID: helper.OperationID}, nil
}

// VerifyMachineControlRequest accepts only the current renewable machine-control
// session. It deliberately has no helper-identity fallback and is the verifier
// for private-access authority.
func (s *EnrollmentService) VerifyMachineControlRequest(ctx context.Context, identityToken string, proof []byte, method, path string, body []byte) (MachineRequestClaims, error) {
	now := s.clock().UTC()
	identity, err := s.signer.VerifyCredential(identityToken, s.issuer, "machine_control", now)
	if err != nil {
		return MachineRequestClaims{}, proofRejected("control_credential_invalid")
	}
	machine, err := s.store.Queries().GetActiveUserMachineForControl(ctx, identity.MachineID)
	if err != nil {
		return MachineRequestClaims{}, machineProofLookupError(err)
	}
	if machine.InstallationGeneration != identity.InstallationGeneration {
		return MachineRequestClaims{}, proofRejected("installation_generation_moved")
	}
	if machine.UserID != identity.UserID || machine.EnvironmentID != identity.EnvironmentID || machineControlThumbprint(machine) != identity.KeyThumbprint {
		return MachineRequestClaims{}, proofRejected("control_credential_mismatch")
	}
	current, err := s.store.Queries().GetCurrentMachineControlSession(ctx, dbsqlc.GetCurrentMachineControlSessionParams{
		MachineID: machine.ID, InstallationGeneration: machine.InstallationGeneration,
		CredentialJti: identity.JTI, Now: now,
	})
	if err != nil {
		return MachineRequestClaims{}, machineProofLookupError(err)
	}
	if current.SessionGeneration != identity.SessionGeneration || current.OperationID == "" {
		return MachineRequestClaims{}, proofRejected("control_session_superseded")
	}
	claims, err := verifyCanonicalMachineProof(machine, proof, method, path, body, now)
	if err != nil {
		return MachineRequestClaims{}, err
	}
	if err := s.reserveProofNonce(ctx, "machine:"+machine.ID, claims.Nonce, method, claims.ExpiresAt); err != nil {
		return MachineRequestClaims{}, err
	}
	return MachineRequestClaims{
		MachineID: machine.ID, EnvironmentID: machine.EnvironmentID, UserID: machine.UserID,
		InstallationGeneration: machine.InstallationGeneration, OperationID: claims.OperationID,
		CredentialJTI: current.CredentialJti, SessionGeneration: current.SessionGeneration,
	}, nil
}

func machineProofLookupError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return proofRejected("machine_not_active")
	}
	return err
}

// ProofRejection is an invalid machine credential or proof, with a fixed reason code (no request or credential
// content) so a rejection log says which check refused it. It is ErrHelperProof for every caller that branches.
type ProofRejection struct{ Reason string }

func (e ProofRejection) Error() string        { return "machine proof rejected: " + e.Reason }
func (e ProofRejection) Is(target error) bool { return target == ErrHelperProof }

func proofRejected(reason string) error { return ProofRejection{Reason: reason} }

// ProofRejectionReason is the reason code of a rejected machine proof, or "" when err is not one.
func ProofRejectionReason(err error) string {
	var rejection ProofRejection
	if errors.As(err, &rejection) {
		return rejection.Reason
	}
	if errors.Is(err, ErrHelperProof) {
		return "proof_rejected"
	}
	return ""
}

func verifyCanonicalMachineProof(machine dbsqlc.UserMachine, encoded []byte, method, path string, body []byte, now time.Time) (canonicalMachineProofClaims, error) {
	if len(encoded) == 0 || len(encoded) > 16<<10 || !machine.PublicIdentityKey.Valid {
		return canonicalMachineProofClaims{}, proofRejected("proof_malformed")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(machine.PublicIdentityKey.String)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return canonicalMachineProofClaims{}, proofRejected("machine_key_invalid")
	}
	var envelope helperProofEnvelope
	if strictProofJSON(encoded, &envelope) != nil || envelope.Algorithm != "EdDSA" {
		return canonicalMachineProofClaims{}, proofRejected("proof_malformed")
	}
	payload, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
	if err != nil || len(payload) > 16<<10 {
		return canonicalMachineProofClaims{}, proofRejected("proof_malformed")
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return canonicalMachineProofClaims{}, proofRejected("proof_signature_invalid")
	}
	var claims canonicalMachineProofClaims
	if strictCanonicalMachineJSON(payload, &claims) != nil {
		return canonicalMachineProofClaims{}, proofRejected("proof_malformed")
	}
	digest := sha256.Sum256(body)
	if claims.InstallationGeneration != machine.InstallationGeneration {
		return canonicalMachineProofClaims{}, proofRejected("installation_generation_moved")
	}
	if claims.MachineID != machine.ID || claims.EnvironmentID != machine.EnvironmentID || len(claims.OperationID) < 8 || len(claims.OperationID) > 128 || claims.Method != strings.ToUpper(method) || claims.Path != path || claims.BodySHA256 != base64.RawURLEncoding.EncodeToString(digest[:]) {
		return canonicalMachineProofClaims{}, proofRejected("proof_request_mismatch")
	}
	if err := proofWindow(claims.IssuedAt, claims.ExpiresAt, now); err != nil {
		return canonicalMachineProofClaims{}, err
	}
	return claims, nil
}

func machineControlThumbprint(machine dbsqlc.UserMachine) string {
	publicKey, err := base64.RawURLEncoding.DecodeString(machine.PublicIdentityKey.String)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return ""
	}
	digest := sha256.Sum256(publicKey)
	return "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func strictCanonicalMachineJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrHelperProof
	}
	return nil
}

// ProofClockWindow is how far a signed proof's issued_at may be ahead of the server clock.
const ProofClockWindow = time.Minute

// ErrClockSkew rejects a correctly signed proof whose time window does not contain the server's
// clock: the machine's clock is off (or the proof is stale). It is still a proof denial (it wraps
// ErrHelperProof), but answered with its own code, not 401, so a client corrects its clock instead of
// treating the machine as signed out.
var ErrClockSkew = fmt.Errorf("signed proof time is outside the server's clock window: %w", ErrHelperProof)

// proofWindow checks a proof's validity window: a malformed window is a plain proof denial; a
// well-formed one that does not contain now is clock skew.
func proofWindow(issued, expires, now time.Time) error {
	if issued.IsZero() || expires.IsZero() || !expires.After(issued) || expires.Sub(issued) > ProofClockWindow {
		return ErrHelperProof
	}
	if issued.After(now.Add(ProofClockWindow)) || !expires.After(now) {
		return ErrClockSkew
	}
	return nil
}
