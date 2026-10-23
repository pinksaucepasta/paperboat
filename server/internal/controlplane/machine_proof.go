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

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
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
	helper, err := s.VerifyHelperRequest(ctx, identityToken, proof, method, path, body)
	if err != nil {
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
		return MachineRequestClaims{}, ErrHelperProof
	}
	machine, err := s.store.Queries().GetActiveUserMachineForControl(ctx, identity.MachineID)
	if err != nil {
		return MachineRequestClaims{}, machineProofLookupError(err)
	}
	if machine.UserID != identity.UserID || machine.EnvironmentID != identity.EnvironmentID || machine.InstallationGeneration != identity.InstallationGeneration || machineControlThumbprint(machine) != identity.KeyThumbprint {
		return MachineRequestClaims{}, ErrHelperProof
	}
	current, err := s.store.Queries().GetCurrentMachineControlSession(ctx, dbsqlc.GetCurrentMachineControlSessionParams{
		MachineID: machine.ID, InstallationGeneration: machine.InstallationGeneration,
		CredentialJti: identity.JTI, Now: now,
	})
	if err != nil {
		return MachineRequestClaims{}, machineProofLookupError(err)
	}
	if current.SessionGeneration != identity.SessionGeneration || current.OperationID == "" {
		return MachineRequestClaims{}, ErrHelperProof
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
		return ErrHelperProof
	}
	return err
}

func verifyCanonicalMachineProof(machine dbsqlc.UserMachine, encoded []byte, method, path string, body []byte, now time.Time) (canonicalMachineProofClaims, error) {
	if len(encoded) == 0 || len(encoded) > 16<<10 || !machine.PublicIdentityKey.Valid {
		return canonicalMachineProofClaims{}, ErrHelperProof
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(machine.PublicIdentityKey.String)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return canonicalMachineProofClaims{}, ErrHelperProof
	}
	var envelope helperProofEnvelope
	if strictProofJSON(encoded, &envelope) != nil || envelope.Algorithm != "EdDSA" {
		return canonicalMachineProofClaims{}, ErrHelperProof
	}
	payload, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
	if err != nil || len(payload) > 16<<10 {
		return canonicalMachineProofClaims{}, ErrHelperProof
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return canonicalMachineProofClaims{}, ErrHelperProof
	}
	var claims canonicalMachineProofClaims
	if strictCanonicalMachineJSON(payload, &claims) != nil {
		return canonicalMachineProofClaims{}, ErrHelperProof
	}
	digest := sha256.Sum256(body)
	if claims.MachineID != machine.ID || claims.EnvironmentID != machine.EnvironmentID || claims.InstallationGeneration != machine.InstallationGeneration || len(claims.OperationID) < 8 || len(claims.OperationID) > 128 || claims.Method != strings.ToUpper(method) || claims.Path != path || claims.BodySHA256 != base64.RawURLEncoding.EncodeToString(digest[:]) {
		return canonicalMachineProofClaims{}, ErrHelperProof
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
