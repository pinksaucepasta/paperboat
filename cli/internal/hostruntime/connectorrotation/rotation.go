// Package connectorrotation owns the host-side half of credential rotation.
//
// The control protocol owns the wire messages and the server owns policy and
// durable operation state. This package owns the small, security-sensitive
// boundary between those messages and a host credential store. In particular,
// private keys never enter a protocol message, a host-state snapshot, or a
// recovery record. A KeyStore implementation is expected to keep the private
// key in the platform credential facility and expose only signing by
// reference.
package connectorrotation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

type Manager struct {
	mu     sync.Mutex
	config Config
	record *Record
}

func New(config Config) (*Manager, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &Manager{config: config}, nil
}

// Recover loads one operation and verifies every durable identity binding. It
// does not issue a message or mutate the key store, so calling it repeatedly
// after a process restart is safe.
func (m *Manager) Recover(ctx context.Context, operationID string) error {
	if m == nil || ctx == nil || connectorprotocol.ValidateIdentifier(operationID) != nil {
		return ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := m.config.Journal.Load(ctx, operationID)
	if err != nil {
		return safeRotationFailure("connector rotation record could not be recovered", err)
	}
	if err := record.Validate(m.config.Clock.Now().UTC()); err != nil {
		return err
	}
	if record.OperationID != operationID || !m.identityMatches(record) {
		return ErrOperationConflict
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record != nil && m.record.OperationID != record.OperationID {
		return ErrOperationConflict
	}
	copy := record.clone()
	m.record = &copy
	return nil
}

func (m *Manager) identityMatches(record Record) bool {
	return record.AccountID == m.config.AccountID && record.TunnelID == m.config.TunnelID && record.ConnectorID == m.config.ConnectorID && record.HostID == m.config.HostID && record.OldCredentialReference == m.config.OldCredentialReference && record.OldIdentityKeyID == m.config.OldIdentityKeyID && record.OldIdentityThumbprint == m.config.OldIdentityThumbprint && record.OldCredentialGeneration == m.config.OldCredentialGeneration
}

// Record returns only safe restart metadata. It never returns a private key.
func (m *Manager) Record() (Record, bool) {
	if m == nil {
		return Record{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record == nil {
		return Record{}, false
	}
	return m.record.clone(), true
}

func (m *Manager) AcceptChallenge(ctx context.Context, challenge connectorprotocol.CredentialRotationChallenge) (connectorprotocol.CredentialRotationProof, error) {
	if m == nil || ctx == nil {
		return connectorprotocol.CredentialRotationProof{}, ErrInvalidConfig
	}
	now := m.config.Clock.Now().UTC()
	if err := challenge.Validate(now); err != nil {
		return connectorprotocol.CredentialRotationProof{}, err
	}
	if !m.challengeIdentityMatches(challenge) {
		return connectorprotocol.CredentialRotationProof{}, typed(connectorprotocol.CodeIdentityMismatch, connectorprotocol.ReasonAuthentication, false, ErrOperationConflict)
	}
	if err := ctx.Err(); err != nil {
		return connectorprotocol.CredentialRotationProof{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record == nil {
		persisted, loadErr := m.config.Journal.Load(ctx, challenge.OperationID)
		switch {
		case loadErr == nil:
			if persisted.Validate(time.Time{}) != nil || !m.identityMatches(persisted) {
				return connectorprotocol.CredentialRotationProof{}, ErrOperationConflict
			}
			copy := persisted.clone()
			m.record = &copy
		case !errorLeavesMatch(loadErr, ErrOperationNotFound):
			return connectorprotocol.CredentialRotationProof{}, safeRotationFailure("credential rotation journal could not be loaded", loadErr)
		}
	}
	if m.record != nil {
		if !m.sameChallengeTarget(*m.record.Challenge, challenge) {
			return connectorprotocol.CredentialRotationProof{}, ErrOperationConflict
		}
		if m.record.Phase == PhaseRevoked {
			return connectorprotocol.CredentialRotationProof{}, typed(connectorprotocol.CodeStaleGeneration, connectorprotocol.ReasonStaleGeneration, false, ErrAlreadyTerminal)
		}
		if m.record.Phase != PhaseChallenged && m.record.Phase != PhaseProofAccepted {
			return connectorprotocol.CredentialRotationProof{}, typed(connectorprotocol.CodeStaleGeneration, connectorprotocol.ReasonStaleGeneration, false, ErrStaleMessage)
		}
		if !m.sameChallenge(*m.record.Challenge, challenge) {
			updated := m.record.clone()
			updated.Challenge = cloneChallenge(challenge)
			updated.Install = nil
			updated.Ready = nil
			updated.Revoke = nil
			updated.Phase = PhaseChallenged
			updated.UpdatedAt = now
			if err := m.saveLocked(ctx, updated); err != nil {
				return connectorprotocol.CredentialRotationProof{}, err
			}
		}
	} else {
		newKey, err := GenerateReplacementKey(ctx, m.config.KeyStore, m.config.Random)
		if err != nil {
			return connectorprotocol.CredentialRotationProof{}, typed(connectorprotocol.CodeCredentialRotationFailed, connectorprotocol.ReasonCredentialRotation, true, err)
		}
		record := Record{
			Version: journalVersion, AccountID: challenge.AccountID, TunnelID: challenge.TunnelID,
			OperationID: challenge.OperationID, ConnectorID: challenge.ConnectorID, HostID: challenge.HostID,
			TargetSetHash: challenge.TargetSetHash, OldCredentialReference: m.config.OldCredentialReference,
			OldIdentityKeyID: challenge.OldIdentityKeyID, OldIdentityThumbprint: challenge.OldIdentityKeyThumbprint,
			OldCredentialGeneration: challenge.OldCredentialGeneration, NewCredentialGeneration: challenge.NewCredentialGeneration,
			NewKey: newKey, Challenge: cloneChallenge(challenge), Phase: PhaseChallenged, UpdatedAt: now,
		}
		if err := record.Validate(time.Time{}); err != nil {
			cleanupErr := m.config.KeyStore.Delete(context.Background(), newKey.Reference)
			return connectorprotocol.CredentialRotationProof{}, safeRotationFailure("replacement credential could not be committed", errors.Join(err, cleanupErr))
		}
		if err := m.config.Journal.Save(ctx, record); err != nil {
			cleanupErr := m.config.KeyStore.Delete(context.Background(), newKey.Reference)
			cause := safeRotationFailure("replacement credential could not be committed", errors.Join(err, cleanupErr))
			return connectorprotocol.CredentialRotationProof{}, typed(connectorprotocol.CodeCredentialRotationFailed, connectorprotocol.ReasonCredentialRotation, true, cause)
		}
		m.record = &record
	}
	proof, err := m.signProofLocked(ctx, *m.record)
	if err != nil {
		return connectorprotocol.CredentialRotationProof{}, err
	}
	if m.record.Phase == PhaseChallenged {
		next := m.record.clone()
		next.Phase = PhaseProofAccepted
		next.UpdatedAt = now
		if err := m.saveLocked(ctx, next); err != nil {
			return connectorprotocol.CredentialRotationProof{}, err
		}
	}
	return proof, nil
}

func (m *Manager) challengeIdentityMatches(challenge connectorprotocol.CredentialRotationChallenge) bool {
	return challenge.AccountID == m.config.AccountID && challenge.TunnelID == m.config.TunnelID && challenge.ConnectorID == m.config.ConnectorID && challenge.HostID == m.config.HostID && challenge.OldCredentialGeneration == m.config.OldCredentialGeneration && challenge.OldIdentityKeyID == m.config.OldIdentityKeyID && challenge.OldIdentityKeyThumbprint == m.config.OldIdentityThumbprint
}

func (m *Manager) signProofLocked(ctx context.Context, record Record) (connectorprotocol.CredentialRotationProof, error) {
	publicKey := base64.RawURLEncoding.EncodeToString(record.NewKey.PublicKey)
	proof := connectorprotocol.CredentialRotationProof{
		AccountID: record.AccountID, TunnelID: record.TunnelID, OperationID: record.OperationID,
		ConnectorID: record.ConnectorID, HostID: record.HostID, SessionID: record.Challenge.SessionID,
		ProcessGeneration: record.Challenge.ProcessGeneration, TargetSetHash: record.TargetSetHash,
		OldCredentialGeneration: record.OldCredentialGeneration, NewCredentialGeneration: record.NewCredentialGeneration,
		OldIdentityKeyID: record.OldIdentityKeyID, OldIdentityKeyThumbprint: record.OldIdentityThumbprint,
		NewIdentityKeyID: record.NewKey.KeyID, NewIdentityKeyThumbprint: record.NewKey.Thumbprint,
		NewPublicKey: publicKey, NewCredentialReference: record.NewKey.Reference,
		ChallengeNonce: record.Challenge.ChallengeNonce, IssuedAt: record.Challenge.IssuedAt,
		NewCredentialValidUntil: record.Challenge.NewCredentialValidUntil,
	}
	payload, err := connectorprotocol.CredentialRotationProofPayload(proof)
	if err != nil {
		return connectorprotocol.CredentialRotationProof{}, err
	}
	oldSignature, err := m.config.KeyStore.Sign(ctx, m.config.OldCredentialReference, payload)
	if err != nil {
		if errorLeavesMatch(err, ErrKeyUnavailable) {
			cause := safeRotationFailure("old connector credential is unavailable", err)
			return connectorprotocol.CredentialRotationProof{}, typed(connectorprotocol.CodeAuthenticationFailed, connectorprotocol.ReasonAuthentication, true, cause)
		}
		cause := safeRotationFailure("old connector credential could not sign the rotation proof", err)
		return connectorprotocol.CredentialRotationProof{}, typed(connectorprotocol.CodeCredentialRotationFailed, connectorprotocol.ReasonCredentialRotation, true, cause)
	}
	newSignature, err := m.config.KeyStore.Sign(ctx, record.NewKey.Reference, payload)
	if err != nil {
		cause := safeRotationFailure("replacement connector credential could not sign the rotation proof", errors.Join(ErrKeyUnavailable, err))
		return connectorprotocol.CredentialRotationProof{}, typed(connectorprotocol.CodeCredentialRotationFailed, connectorprotocol.ReasonCredentialRotation, true, cause)
	}
	proof.OldSignedProof = base64.RawURLEncoding.EncodeToString(oldSignature)
	proof.NewSignedProof = base64.RawURLEncoding.EncodeToString(newSignature)
	if err := proof.Validate(nowUTC(m.config.Clock)); err != nil {
		return connectorprotocol.CredentialRotationProof{}, err
	}
	return proof, nil
}

func (m *Manager) AcceptInstall(ctx context.Context, install connectorprotocol.CredentialRotationInstall) (connectorprotocol.CredentialRotationAck, error) {
	if m == nil || ctx == nil {
		return connectorprotocol.CredentialRotationAck{}, ErrInvalidConfig
	}
	now := nowUTC(m.config.Clock)
	if err := install.Validate(now); err != nil {
		return connectorprotocol.CredentialRotationAck{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record == nil {
		return connectorprotocol.CredentialRotationAck{}, ErrOperationNotFound
	}
	record := *m.record
	if !record.installMatches(install) || install.NewCredentialValidUntil.Before(now) {
		return connectorprotocol.CredentialRotationAck{}, typed(connectorprotocol.CodeIdentityMismatch, connectorprotocol.ReasonAuthentication, false, ErrOperationConflict)
	}
	if phaseAtLeast(record.Phase, PhaseInstalled) {
		return installAck(install), nil
	}
	if record.Phase != PhaseProofAccepted {
		return connectorprotocol.CredentialRotationAck{}, typed(connectorprotocol.CodeStaleGeneration, connectorprotocol.ReasonStaleGeneration, false, ErrStaleMessage)
	}
	if err := ctx.Err(); err != nil {
		return connectorprotocol.CredentialRotationAck{}, err
	}
	if err := m.config.Installer.Install(ctx, install); err != nil {
		cause := safeRotationFailure("replacement connector could not be installed", errors.Join(ErrInstallationFailed, err))
		return connectorprotocol.CredentialRotationAck{}, typed(connectorprotocol.CodeCredentialRotationFailed, connectorprotocol.ReasonCredentialRotation, true, cause)
	}
	record.Install = cloneInstall(install)
	record.Phase = PhaseInstalled
	record.UpdatedAt = now
	if err := m.saveLocked(ctx, record); err != nil {
		return connectorprotocol.CredentialRotationAck{}, err
	}
	return installAck(install), nil
}

// MarkReady emits credential-aware replacement-session readiness. It refuses
// the old process generation and carries the exact replacement key identity,
// reference, configuration generation, and content hash.
func (m *Manager) MarkReady(ctx context.Context, sessionID string, processGeneration, configGeneration uint64, configContentHash string, edgeReady, routeReady, originReady bool) (connectorprotocol.CredentialRotationReady, error) {
	if m == nil || ctx == nil || connectorprotocol.ValidateIdentifier(sessionID) != nil {
		return connectorprotocol.CredentialRotationReady{}, ErrInvalidConfig
	}
	now := nowUTC(m.config.Clock)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record == nil || m.record.Install == nil {
		return connectorprotocol.CredentialRotationReady{}, ErrOperationNotFound
	}
	record := *m.record
	if phaseAtLeast(record.Phase, PhaseReady) {
		if record.Ready != nil && record.Ready.SessionID == sessionID && record.Ready.ProcessGeneration == processGeneration && record.Ready.ConfigGeneration == configGeneration && record.Ready.ConfigContentHash == configContentHash {
			return *record.Ready, nil
		}
		return connectorprotocol.CredentialRotationReady{}, typed(connectorprotocol.CodeStaleGeneration, connectorprotocol.ReasonStaleGeneration, false, ErrStaleMessage)
	}
	if record.Phase != PhaseInstalled || sessionID == record.Install.SessionID || processGeneration < record.Install.ReplacementProcessGeneration {
		return connectorprotocol.CredentialRotationReady{}, typed(connectorprotocol.CodeIdentityMismatch, connectorprotocol.ReasonAuthentication, false, ErrOperationConflict)
	}
	if configGeneration == 0 || !validSHA256Hash(configContentHash) {
		return connectorprotocol.CredentialRotationReady{}, ErrInvalidConfig
	}
	ready := connectorprotocol.CredentialRotationReady{
		AccountID: record.AccountID, TunnelID: record.TunnelID, OperationID: record.OperationID,
		ConnectorID: record.ConnectorID, HostID: record.HostID, SessionID: sessionID,
		PreviousSessionID: record.Install.SessionID, ProcessGeneration: processGeneration,
		TargetSetHash: record.TargetSetHash, OldCredentialGeneration: record.OldCredentialGeneration,
		NewCredentialGeneration: record.NewCredentialGeneration, NewIdentityKeyID: record.NewKey.KeyID,
		NewIdentityKeyThumbprint: record.NewKey.Thumbprint,
		NewPublicKey:             base64.RawURLEncoding.EncodeToString(record.NewKey.PublicKey),
		NewCredentialReference:   record.NewKey.Reference, NewCredentialValidUntil: record.Install.NewCredentialValidUntil,
		ConfigGeneration: configGeneration, ConfigContentHash: configContentHash,
		EdgeReady: edgeReady, RouteReady: routeReady, OriginReady: originReady, ReadyAt: now,
	}
	if err := ready.Validate(now); err != nil {
		return connectorprotocol.CredentialRotationReady{}, err
	}
	if err := ctx.Err(); err != nil {
		return connectorprotocol.CredentialRotationReady{}, err
	}
	record.Ready = cloneReady(ready)
	record.Phase = PhaseReady
	record.UpdatedAt = now
	if err := m.saveLocked(ctx, record); err != nil {
		return connectorprotocol.CredentialRotationReady{}, err
	}
	return ready, nil
}

// AcceptRevoke deletes the old private key only after exact operation,
// connector, target-generation, replacement-session, and process-generation
// checks. New key material remains available for the replacement session.
func (m *Manager) AcceptRevoke(ctx context.Context, revoke connectorprotocol.CredentialRotationRevoke) (connectorprotocol.CredentialRotationAck, error) {
	if m == nil || ctx == nil {
		return connectorprotocol.CredentialRotationAck{}, ErrInvalidConfig
	}
	now := nowUTC(m.config.Clock)
	if err := revoke.Validate(now); err != nil {
		return connectorprotocol.CredentialRotationAck{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record == nil || m.record.Ready == nil {
		return connectorprotocol.CredentialRotationAck{}, ErrOperationNotFound
	}
	record := *m.record
	if record.Phase == PhaseRevoked {
		if record.Revoke != nil && record.revokeMatches(revoke) {
			return revokeAck(revoke), nil
		}
		return connectorprotocol.CredentialRotationAck{}, typed(connectorprotocol.CodeStaleGeneration, connectorprotocol.ReasonStaleGeneration, false, ErrAlreadyTerminal)
	}
	if (record.Phase != PhaseReady && record.Phase != PhaseRevoking) || !record.revokeMatches(revoke) {
		return connectorprotocol.CredentialRotationAck{}, typed(connectorprotocol.CodeIdentityMismatch, connectorprotocol.ReasonAuthentication, false, ErrOperationConflict)
	}
	if record.Phase == PhaseReady {
		record.Revoke = cloneRevoke(revoke)
		record.Phase = PhaseRevoking
		record.UpdatedAt = now
		if err := m.saveLocked(ctx, record); err != nil {
			return connectorprotocol.CredentialRotationAck{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return connectorprotocol.CredentialRotationAck{}, err
	}
	if err := m.config.Installer.Revoke(ctx, revoke); err != nil {
		cause := safeRotationFailure("old connector could not be revoked", errors.Join(ErrRevocationFailed, err))
		return connectorprotocol.CredentialRotationAck{}, typed(connectorprotocol.CodeCredentialRotationFailed, connectorprotocol.ReasonCredentialRotation, true, cause)
	}
	if err := m.config.KeyStore.Delete(ctx, m.config.OldCredentialReference); err != nil {
		cause := safeRotationFailure("old connector credential could not be removed", errors.Join(ErrRevocationFailed, err))
		return connectorprotocol.CredentialRotationAck{}, typed(connectorprotocol.CodeCredentialRotationFailed, connectorprotocol.ReasonCredentialRotation, true, cause)
	}
	record.Phase = PhaseRevoked
	record.UpdatedAt = nowUTC(m.config.Clock)
	if err := m.saveLocked(ctx, record); err != nil {
		return connectorprotocol.CredentialRotationAck{}, err
	}
	return revokeAck(revoke), nil
}

func (m *Manager) sameChallenge(a, b connectorprotocol.CredentialRotationChallenge) bool {
	return a.AccountID == b.AccountID && a.TunnelID == b.TunnelID && a.OperationID == b.OperationID && a.ConnectorID == b.ConnectorID && a.HostID == b.HostID && a.SessionID == b.SessionID && a.ProcessGeneration == b.ProcessGeneration && a.TargetSetHash == b.TargetSetHash && a.Target == b.Target && a.OldCredentialGeneration == b.OldCredentialGeneration && a.NewCredentialGeneration == b.NewCredentialGeneration && a.OldIdentityKeyID == b.OldIdentityKeyID && a.OldIdentityKeyThumbprint == b.OldIdentityKeyThumbprint && a.ChallengeNonce == b.ChallengeNonce && a.IssuedAt.Equal(b.IssuedAt) && a.ExpiresAt.Equal(b.ExpiresAt) && a.OverlapUntil.Equal(b.OverlapUntil) && a.NewCredentialValidUntil.Equal(b.NewCredentialValidUntil)
}

func (m *Manager) sameChallengeTarget(a, b connectorprotocol.CredentialRotationChallenge) bool {
	return a.AccountID == b.AccountID && a.TunnelID == b.TunnelID && a.OperationID == b.OperationID && a.ConnectorID == b.ConnectorID && a.HostID == b.HostID && a.TargetSetHash == b.TargetSetHash && a.Target == b.Target && a.OldCredentialGeneration == b.OldCredentialGeneration && a.NewCredentialGeneration == b.NewCredentialGeneration && a.OldIdentityKeyID == b.OldIdentityKeyID && a.OldIdentityKeyThumbprint == b.OldIdentityKeyThumbprint && a.OverlapUntil.Equal(b.OverlapUntil) && a.NewCredentialValidUntil.Equal(b.NewCredentialValidUntil)
}

func (m *Manager) saveLocked(ctx context.Context, record Record) error {
	if err := record.Validate(time.Time{}); err != nil {
		return err
	}
	if err := m.config.Journal.Save(ctx, record); err != nil {
		cause := safeRotationFailure("credential rotation journal could not be saved", errors.Join(ErrRecoveryRequired, err))
		return typed(connectorprotocol.CodeCredentialRotationFailed, connectorprotocol.ReasonCredentialRotation, true, cause)
	}
	copy := record.clone()
	m.record = &copy
	return nil
}
