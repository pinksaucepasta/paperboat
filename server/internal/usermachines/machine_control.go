package usermachines

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

var ErrMachineControlInvalid = errors.New("machine control credential request is invalid")

const machineControlTTL = time.Hour

type MachineControlCredential struct {
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type machineProofEnvelope struct {
	Algorithm string `json:"alg"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type MachineProofClaims struct {
	MachineID              string    `json:"machine_id"`
	EnvironmentID          string    `json:"environment_id"`
	InstallationGeneration int64     `json:"installation_generation"`
	OperationID            string    `json:"operation_id"`
	Method                 string    `json:"method"`
	Path                   string    `json:"path"`
	BodySHA256             string    `json:"body_sha256"`
	IssuedAt               time.Time `json:"issued_at"`
	ExpiresAt              time.Time `json:"expires_at"`
	// Nonce makes the proof one-use (F21-13). Clients since b684f426 send it on every proof; this strict
	// decoder refused the unknown field, so every machine-control renewal answered 401.
	Nonce string `json:"nonce,omitempty"`
}

func (s *Service) ConfigureMachineControl(signer *mint.Provider, issuer string) {
	s.controlSigner = signer
	s.issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
}

func (s *Service) IssueMachineControl(ctx context.Context, userID, machineID string, proof, body []byte, method, path string) (MachineControlCredential, error) {
	if s.controlSigner == nil || strings.TrimSpace(userID) == "" {
		return MachineControlCredential{}, ErrMachineControlInvalid
	}
	machine, err := s.db.Queries().GetOwnedActiveUserMachineForControl(ctx, dbsqlc.GetOwnedActiveUserMachineForControlParams{ID: machineID, UserID: userID})
	if err != nil {
		return MachineControlCredential{}, machineControlLookupError(err)
	}
	claims, err := verifyMachineProof(machine, proof, method, path, body, s.now().UTC())
	if err != nil {
		return MachineControlCredential{}, err
	}
	if err := s.reserveMachineProofNonce(ctx, machine.ID, claims, method); err != nil {
		return MachineControlCredential{}, err
	}
	connector, err := s.db.Queries().EnsureControlConnectorMachine(ctx, dbsqlc.EnsureControlConnectorMachineParams{EnvironmentID: machine.EnvironmentID, ConnectorID: "runtime", MachineID: machine.ID, EdgePool: "default"})
	if err != nil {
		return MachineControlCredential{}, err
	}
	if connector.MachineID != machine.ID {
		return MachineControlCredential{}, ErrMachineControlInvalid
	}
	return s.mintMachineControl(ctx, machine, claims.OperationID)
}

// IssueInitialMachineControl exchanges an already verified helper identity for
// the first machine-control credential. The HTTP boundary verifies that the
// helper credential and proof are bound to this exact machine key; this method
// then repeats the durable machine and connector binding checks before minting.
// It intentionally does not accept a user session because one-shot bootstrap
// may install the runtime before a local CLI session is usable.
func (s *Service) IssueInitialMachineControl(ctx context.Context, machineID, environmentID, operationID string) (MachineControlCredential, error) {
	if s.controlSigner == nil || strings.TrimSpace(machineID) == "" || strings.TrimSpace(environmentID) == "" || len(operationID) < 8 || len(operationID) > 128 {
		return MachineControlCredential{}, ErrMachineControlInvalid
	}
	machine, err := s.db.Queries().GetActiveUserMachineForControl(ctx, machineID)
	// Bind initial control credentials to the exact active enrolled machine.
	if err != nil {
		return MachineControlCredential{}, machineControlLookupError(err)
	}
	if machine.EnvironmentID != environmentID {
		return MachineControlCredential{}, ErrMachineControlInvalid
	}
	connector, err := s.db.Queries().EnsureControlConnectorMachine(ctx, dbsqlc.EnsureControlConnectorMachineParams{EnvironmentID: machine.EnvironmentID, ConnectorID: "runtime", MachineID: machine.ID, EdgePool: "default"})
	if err != nil {
		return MachineControlCredential{}, err
	}
	if connector.MachineID != machine.ID {
		return MachineControlCredential{}, ErrMachineControlInvalid
	}
	return s.mintMachineControl(ctx, machine, operationID)
}

func (s *Service) RenewMachineControl(ctx context.Context, credential string, proof, body []byte, method, path string) (MachineControlCredential, error) {
	if s.controlSigner == nil {
		return MachineControlCredential{}, ErrMachineControlInvalid
	}
	now := s.now().UTC()
	identity, err := s.controlSigner.VerifyCredentialWithExpiryGrace(credential, s.issuer, "machine_control", now, time.Hour)
	if err != nil {
		var verification mint.CredentialVerificationError
		if errors.As(err, &verification) {
			return MachineControlCredential{}, controlRejected("credential_" + string(verification.Code))
		}
		return MachineControlCredential{}, controlRejected("credential_invalid")
	}
	machine, err := s.db.Queries().GetActiveUserMachineForControl(ctx, identity.MachineID)
	if err != nil {
		return MachineControlCredential{}, machineControlLookupError(err)
	}
	switch {
	case machine.InstallationGeneration != identity.InstallationGeneration:
		return MachineControlCredential{}, controlRejected("installation_generation_moved")
	case machineKeyThumbprint(machine) != identity.KeyThumbprint:
		return MachineControlCredential{}, controlRejected("machine_key_changed")
	case machine.UserID != identity.UserID || machine.EnvironmentID != identity.EnvironmentID:
		return MachineControlCredential{}, controlRejected("binding_mismatch")
	}
	current, err := s.db.Queries().GetCurrentMachineControlSession(ctx, dbsqlc.GetCurrentMachineControlSessionParams{
		MachineID: machine.ID, InstallationGeneration: machine.InstallationGeneration,
		CredentialJti: identity.JTI, Now: now.Add(-time.Hour),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return s.replayMachineControlRenewal(ctx, machine, identity.JTI, proof, body, method, path, now)
	}
	if err != nil {
		return MachineControlCredential{}, err
	}
	if current.SessionGeneration != identity.SessionGeneration {
		return MachineControlCredential{}, controlRejected("session_superseded")
	}
	claims, err := verifyMachineProof(machine, proof, method, path, body, now)
	if err != nil {
		return MachineControlCredential{}, controlReason("proof", err)
	}
	if err := s.reserveMachineProofNonce(ctx, machine.ID, claims, method); err != nil {
		return MachineControlCredential{}, err
	}
	out, err := s.mintMachineControl(ctx, machine, claims.OperationID)
	return out, controlReason("mint", err)
}

// reserveMachineProofNonce uses a proof's nonce once (db.ReserveProofNonce, the signer namespace every
// machine-proof verifier shares). The replay path does not reserve: a retry of a committed renewal carries
// the same proof by design and is bound to its operation instead.
func (s *Service) reserveMachineProofNonce(ctx context.Context, machineID string, claims MachineProofClaims, method string) error {
	ok, err := s.db.ReserveProofNonce(ctx, "machine:"+machineID, claims.Nonce, method, claims.ExpiresAt)
	if err != nil {
		return err
	}
	if !ok {
		return controlRejected("proof_nonce_replayed")
	}
	return nil
}

// ControlRejection is a refused machine-control request with a fixed reason code (no credential or request
// content), so a 401 log says which check refused it. It is ErrMachineControlInvalid for every caller.
type ControlRejection struct{ Reason string }

func (e ControlRejection) Error() string        { return "machine control rejected: " + e.Reason }
func (e ControlRejection) Is(target error) bool { return target == ErrMachineControlInvalid }

// ProofClockSkewReason is the rejection of a signed proof made with a clock outside the window.
const ProofClockSkewReason = "proof_clock_skew"

func controlRejected(reason string) error { return ControlRejection{Reason: reason} }

// controlReason tags an untyped ErrMachineControlInvalid from a stage with that stage; other errors pass.
func controlReason(stage string, err error) error {
	var typed ControlRejection
	if err == nil || errors.As(err, &typed) || !errors.Is(err, ErrMachineControlInvalid) {
		return err
	}
	return controlRejected(stage + "_rejected")
}

// ControlRejectionReason is the reason code of a refused machine-control request, or "".
func ControlRejectionReason(err error) string {
	var typed ControlRejection
	if errors.As(err, &typed) {
		return typed.Reason
	}
	if errors.Is(err, ErrMachineControlInvalid) {
		return "rejected"
	}
	return ""
}

// replayMachineControlRenewal answers a retry whose first attempt committed but
// whose response was lost: the presented credential was already replaced by the
// renewal with this exact operation, so that renewal's credential is returned
// again until it expires. Any other rotated credential stays invalid.
func (s *Service) replayMachineControlRenewal(ctx context.Context, machine dbsqlc.UserMachine, presentedJTI string, proof, body []byte, method, path string, now time.Time) (MachineControlCredential, error) {
	claims, err := verifyMachineProof(machine, proof, method, path, body, now)
	if err != nil {
		return MachineControlCredential{}, controlRejected("replay_proof_rejected")
	}
	_, err = s.db.Queries().GetReplayableMachineControlRenewal(ctx, dbsqlc.GetReplayableMachineControlRenewalParams{
		PreviousCredentialJti: presentedJTI, OperationID: claims.OperationID, MachineID: machine.ID,
		InstallationGeneration: machine.InstallationGeneration, Now: now,
	})
	if errors.Is(err, sql.ErrNoRows) {
		// The presented credential is no longer the current session's, and no renewal with this operation
		// replaced it: a credential that was rotated away (or a different operation id).
		return MachineControlCredential{}, controlRejected("credential_not_current")
	}
	if err != nil {
		return MachineControlCredential{}, err
	}
	out, err := s.mintMachineControl(ctx, machine, claims.OperationID)
	return out, controlReason("replay_mint", err)
}

func machineControlLookupError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMachineControlInvalid
	}
	return err
}

func (s *Service) mintMachineControl(ctx context.Context, machine dbsqlc.UserMachine, operationID string) (MachineControlCredential, error) {
	issuedAt := s.now().UTC().Truncate(time.Second)
	expiresAt := issuedAt.Add(machineControlTTL)
	var row dbsqlc.MachineControlRenewal
	var session dbsqlc.MachineControlSession
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Queries().DeleteExpiredMachineControlRenewals(ctx, issuedAt.Add(-time.Hour)); err != nil {
			return err
		}
		current, err := tx.Queries().GetMachineControlRenewalForUpdate(ctx, operationID)
		if err == nil {
			return s.resolveMachineControlRenewal(ctx, tx, current, machine, operationID, issuedAt, expiresAt, &row, &session)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// The insert is an idempotent reservation. If another request inserted
		// the operation after the lookup, PostgreSQL returns that committed row
		// through the conflict update; resolve it using the same binding and
		// expiry rules below.
		created, err := tx.Queries().CreateMachineControlRenewal(ctx, dbsqlc.CreateMachineControlRenewalParams{
			OperationID: operationID, MachineID: machine.ID, InstallationGeneration: machine.InstallationGeneration,
			CredentialJti: newID("token"), IssuedAt: issuedAt, ExpiresAt: expiresAt,
		})
		if err != nil {
			return err
		}
		return s.resolveMachineControlRenewal(ctx, tx, created, machine, operationID, issuedAt, expiresAt, &row, &session)
	})
	if err != nil {
		return MachineControlCredential{}, err
	}
	if row.MachineID != machine.ID || row.InstallationGeneration != machine.InstallationGeneration || session.CredentialJti != row.CredentialJti || session.SessionGeneration < 1 {
		return MachineControlCredential{}, ErrMachineControlInvalid
	}
	token, err := s.controlSigner.SignCredential(mint.CredentialInput{
		Issuer: s.issuer, Audience: "paperboat-control", Subject: machine.ID, JTI: row.CredentialJti,
		IssuedAt: row.IssuedAt, ExpiresAt: row.ExpiresAt, CredentialClass: "machine_control",
		Scopes: []string{"machine:connect", "machine:renew"}, EnvironmentID: machine.EnvironmentID,
		MachineID: machine.ID, UserID: machine.UserID, KeyThumbprint: machineKeyThumbprint(machine),
		InstallationGeneration: machine.InstallationGeneration, SessionGeneration: session.SessionGeneration,
	})
	if err != nil {
		return MachineControlCredential{}, err
	}
	return MachineControlCredential{Credential: token, ExpiresAt: row.ExpiresAt}, nil
}

func (s *Service) resolveMachineControlRenewal(ctx context.Context, tx *db.Tx, row dbsqlc.MachineControlRenewal, machine dbsqlc.UserMachine, operationID string, issuedAt, expiresAt time.Time, result *dbsqlc.MachineControlRenewal, resultSession *dbsqlc.MachineControlSession) error {
	if row.OperationID != operationID || row.MachineID != machine.ID || row.InstallationGeneration != machine.InstallationGeneration {
		return ErrMachineControlInvalid
	}
	if row.SupersededAt.Valid {
		return ErrMachineControlInvalid
	}
	current, currentErr := tx.Queries().GetMachineControlSessionForUpdate(ctx, machine.ID)
	if currentErr != nil && !errors.Is(currentErr, sql.ErrNoRows) {
		return currentErr
	}
	if row.SessionGeneration.Valid {
		if currentErr != nil || current.OperationID != operationID || current.CredentialJti != row.CredentialJti || current.SessionGeneration != row.SessionGeneration.Int64 {
			return ErrMachineControlInvalid
		}
		if row.ExpiresAt.After(issuedAt) {
			*result, *resultSession = row, current
			return nil
		}
	}
	if !row.SessionGeneration.Valid && currentErr == nil && current.OperationID == operationID {
		return ErrMachineControlInvalid
	}
	if row.SessionGeneration.Valid {
		var err error
		row, err = tx.Queries().RotateMachineControlRenewal(ctx, dbsqlc.RotateMachineControlRenewalParams{
			OperationID: operationID, MachineID: machine.ID, InstallationGeneration: machine.InstallationGeneration,
			CredentialJti: newID("token"), IssuedAt: issuedAt, ExpiresAt: expiresAt,
		})
		if err != nil {
			return err
		}
	}
	if currentErr != nil {
		var err error
		current, err = tx.Queries().CreateMachineControlSession(ctx, dbsqlc.CreateMachineControlSessionParams{
			MachineID: machine.ID, InstallationGeneration: machine.InstallationGeneration, OperationID: operationID,
			CredentialJti: row.CredentialJti, IssuedAt: row.IssuedAt, ExpiresAt: row.ExpiresAt,
		})
		if err != nil {
			return err
		}
	} else {
		if current.OperationID != operationID {
			updated, err := tx.Queries().MarkMachineControlRenewalSuperseded(ctx, dbsqlc.MarkMachineControlRenewalSupersededParams{
				Now: sql.NullTime{Time: issuedAt, Valid: true}, OperationID: current.OperationID, MachineID: machine.ID,
				SessionGeneration: sql.NullInt64{Int64: current.SessionGeneration, Valid: true},
			})
			if err != nil || updated != 1 {
				return ErrMachineControlInvalid
			}
		}
		var err error
		current, err = tx.Queries().RotateMachineControlSession(ctx, dbsqlc.RotateMachineControlSessionParams{
			InstallationGeneration: machine.InstallationGeneration, OperationID: operationID, CredentialJti: row.CredentialJti,
			IssuedAt: row.IssuedAt, ExpiresAt: row.ExpiresAt, MachineID: machine.ID,
			ExpectedSessionGeneration: current.SessionGeneration, ExpectedCredentialJti: current.CredentialJti,
		})
		if err != nil {
			return err
		}
	}
	bound, err := tx.Queries().BindMachineControlRenewalSession(ctx, dbsqlc.BindMachineControlRenewalSessionParams{
		SessionGeneration: sql.NullInt64{Int64: current.SessionGeneration, Valid: true}, OperationID: operationID,
		MachineID: machine.ID, InstallationGeneration: machine.InstallationGeneration, CredentialJti: row.CredentialJti,
	})
	if err != nil {
		return err
	}
	*result, *resultSession = bound, current
	return nil
}

func verifyMachineProof(machine dbsqlc.UserMachine, encoded []byte, method, path string, body []byte, now time.Time) (MachineProofClaims, error) {
	if len(encoded) == 0 || len(encoded) > 16<<10 || len(body) > 1<<20 || !machine.PublicIdentityKey.Valid {
		return MachineProofClaims{}, ErrMachineControlInvalid
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(machine.PublicIdentityKey.String)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return MachineProofClaims{}, ErrMachineControlInvalid
	}
	var envelope machineProofEnvelope
	if strictMachineJSON(encoded, &envelope) != nil || envelope.Algorithm != "EdDSA" {
		return MachineProofClaims{}, ErrMachineControlInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
	if err != nil || len(payload) > 16<<10 {
		return MachineProofClaims{}, ErrMachineControlInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return MachineProofClaims{}, ErrMachineControlInvalid
	}
	var claims MachineProofClaims
	bodyHash := sha256.Sum256(body)
	if strictMachineJSON(payload, &claims) != nil || claims.MachineID != machine.ID || claims.EnvironmentID != machine.EnvironmentID || claims.InstallationGeneration != machine.InstallationGeneration || len(claims.OperationID) < 8 || len(claims.OperationID) > 128 || claims.Method != method || claims.Path != path || claims.BodySHA256 != base64.RawURLEncoding.EncodeToString(bodyHash[:]) || claims.IssuedAt.IsZero() || claims.ExpiresAt.IsZero() || !claims.ExpiresAt.After(claims.IssuedAt) || claims.ExpiresAt.Sub(claims.IssuedAt) > time.Minute {
		return MachineProofClaims{}, ErrMachineControlInvalid
	}
	// A correctly signed proof whose window misses the server's clock is a clock problem, not a bad
	// credential (F21-02): the machine stays signed in and corrects its time from the answer.
	if claims.IssuedAt.After(now.Add(time.Minute)) || !claims.ExpiresAt.After(now) {
		return MachineProofClaims{}, controlRejected(ProofClockSkewReason)
	}
	return claims, nil
}

func machineKeyThumbprint(machine dbsqlc.UserMachine) string {
	publicKey, err := base64.RawURLEncoding.DecodeString(machine.PublicIdentityKey.String)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return ""
	}
	digest := sha256.Sum256(publicKey)
	return "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func strictMachineJSON(data []byte, target any) error {
	if canonicaljson.RejectDuplicateFields(data) != nil {
		return ErrMachineControlInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrMachineControlInvalid
	}
	return nil
}
