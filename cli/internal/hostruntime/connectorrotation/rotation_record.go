package connectorrotation

import (
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

type Config struct {
	AccountID               string
	TunnelID                string
	ConnectorID             string
	HostID                  string
	OldCredentialReference  string
	OldIdentityKeyID        string
	OldIdentityThumbprint   string
	OldCredentialGeneration uint64
	KeyStore                KeyStore
	Journal                 Journal
	Installer               Installer
	Clock                   Clock
	Random                  io.Reader
}

func (c Config) validate() error {
	if connectorprotocol.ValidateIdentifier(c.AccountID) != nil ||
		connectorprotocol.ValidateIdentifier(c.TunnelID) != nil ||
		connectorprotocol.ValidateIdentifier(c.ConnectorID) != nil ||
		connectorprotocol.ValidateIdentifier(c.HostID) != nil ||
		connectorprotocol.ValidateCredentialReference(c.OldCredentialReference) != nil ||
		connectorprotocol.ValidateIdentityKey(c.OldIdentityKeyID, c.OldIdentityThumbprint) != nil ||
		c.OldCredentialGeneration == 0 || c.KeyStore == nil || c.Journal == nil || c.Installer == nil {
		return ErrInvalidConfig
	}
	return nil
}

// Record is the restart-safe host journal representation. It intentionally
// excludes CredentialRotationProof, because detached signatures are not
// needed for recovery and retaining fewer bytes reduces accidental disclosure.
// All fields are either identity, public key, credential reference, or timing
// metadata. There is no private key, bearer, enrollment token, or secret.
type Record struct {
	Version                 int                                            `json:"version"`
	AccountID               string                                         `json:"account_id"`
	TunnelID                string                                         `json:"tunnel_id"`
	OperationID             string                                         `json:"operation_id"`
	ConnectorID             string                                         `json:"connector_id"`
	HostID                  string                                         `json:"host_id"`
	TargetSetHash           string                                         `json:"target_set_hash"`
	OldCredentialReference  string                                         `json:"old_credential_reference"`
	OldIdentityKeyID        string                                         `json:"old_identity_key_id"`
	OldIdentityThumbprint   string                                         `json:"old_identity_key_thumbprint"`
	OldCredentialGeneration uint64                                         `json:"old_credential_generation"`
	NewCredentialGeneration uint64                                         `json:"new_credential_generation"`
	NewKey                  KeyReference                                   `json:"new_key"`
	Challenge               *connectorprotocol.CredentialRotationChallenge `json:"challenge,omitempty"`
	Install                 *connectorprotocol.CredentialRotationInstall   `json:"install,omitempty"`
	Ready                   *connectorprotocol.CredentialRotationReady     `json:"ready,omitempty"`
	Revoke                  *connectorprotocol.CredentialRotationRevoke    `json:"revoke,omitempty"`
	Phase                   Phase                                          `json:"phase"`
	UpdatedAt               time.Time                                      `json:"updated_at"`
}

func (r Record) Validate(now time.Time) error {
	if r.Version != journalVersion || connectorprotocol.ValidateIdentifier(r.AccountID) != nil ||
		connectorprotocol.ValidateIdentifier(r.TunnelID) != nil || connectorprotocol.ValidateIdentifier(r.OperationID) != nil ||
		connectorprotocol.ValidateIdentifier(r.ConnectorID) != nil || connectorprotocol.ValidateIdentifier(r.HostID) != nil ||
		!validSHA256Hash(r.TargetSetHash) ||
		connectorprotocol.ValidateCredentialReference(r.OldCredentialReference) != nil ||
		connectorprotocol.ValidateIdentityKey(r.OldIdentityKeyID, r.OldIdentityThumbprint) != nil ||
		r.OldCredentialGeneration == 0 || r.NewCredentialGeneration == 0 || r.NewCredentialGeneration != r.OldCredentialGeneration+1 ||
		!validPhase(r.Phase) || r.UpdatedAt.IsZero() {
		return ErrJournalCorrupt
	}
	if err := r.NewKey.Validate(); err != nil {
		return ErrJournalCorrupt
	}
	if r.Challenge == nil {
		return ErrJournalCorrupt
	}
	if err := r.Challenge.Validate(time.Time{}); err != nil ||
		r.Challenge.AccountID != r.AccountID || r.Challenge.TunnelID != r.TunnelID ||
		r.Challenge.OperationID != r.OperationID || r.Challenge.ConnectorID != r.ConnectorID ||
		r.Challenge.HostID != r.HostID || r.Challenge.TargetSetHash != r.TargetSetHash ||
		r.Challenge.OldCredentialGeneration != r.OldCredentialGeneration ||
		r.Challenge.NewCredentialGeneration != r.NewCredentialGeneration ||
		r.Challenge.OldIdentityKeyID != r.OldIdentityKeyID ||
		r.Challenge.OldIdentityKeyThumbprint != r.OldIdentityThumbprint {
		return ErrJournalCorrupt
	}
	if phaseAtLeast(r.Phase, PhaseInstalled) {
		if r.Install == nil || r.Install.Validate(time.Time{}) != nil || !r.installMatches(*r.Install) {
			return ErrJournalCorrupt
		}
	}
	if phaseAtLeast(r.Phase, PhaseReady) {
		if r.Ready == nil || r.Ready.Validate(time.Time{}) != nil || !r.readyMatches(*r.Ready) {
			return ErrJournalCorrupt
		}
	}
	if phaseAtLeast(r.Phase, PhaseRevoking) {
		if r.Revoke == nil || r.Revoke.Validate(time.Time{}) != nil || !r.revokeMatches(*r.Revoke) {
			return ErrJournalCorrupt
		}
	}
	if !now.IsZero() && r.UpdatedAt.After(now.Add(connectorprotocol.MaxClockSkew)) {
		return ErrJournalCorrupt
	}
	return nil
}

func (r Record) installMatches(install connectorprotocol.CredentialRotationInstall) bool {
	return install.AccountID == r.AccountID && install.TunnelID == r.TunnelID && install.OperationID == r.OperationID && install.ConnectorID == r.ConnectorID && install.HostID == r.HostID && install.TargetSetHash == r.TargetSetHash && install.OldCredentialGeneration == r.OldCredentialGeneration && install.NewCredentialGeneration == r.NewCredentialGeneration && install.NewIdentityKeyID == r.NewKey.KeyID && install.NewIdentityKeyThumbprint == r.NewKey.Thumbprint && install.NewCredentialReference == r.NewKey.Reference && equalStringBytes(install.NewPublicKey, r.NewKey.PublicKey) && install.SessionID == r.Challenge.SessionID && install.ProcessGeneration == r.Challenge.ProcessGeneration && install.ChallengeNonce == r.Challenge.ChallengeNonce && install.OverlapUntil.Equal(r.Challenge.OverlapUntil) && install.NewCredentialValidUntil.Equal(r.Challenge.NewCredentialValidUntil)
}

func (r Record) readyMatches(ready connectorprotocol.CredentialRotationReady) bool {
	return ready.AccountID == r.AccountID && ready.TunnelID == r.TunnelID && ready.OperationID == r.OperationID && ready.ConnectorID == r.ConnectorID && ready.HostID == r.HostID && ready.TargetSetHash == r.TargetSetHash && ready.OldCredentialGeneration == r.OldCredentialGeneration && ready.NewCredentialGeneration == r.NewCredentialGeneration && ready.NewIdentityKeyID == r.NewKey.KeyID && ready.NewIdentityKeyThumbprint == r.NewKey.Thumbprint && ready.NewCredentialReference == r.NewKey.Reference && equalStringBytes(ready.NewPublicKey, r.NewKey.PublicKey) && ready.PreviousSessionID == r.Install.SessionID && ready.ProcessGeneration == r.Install.ReplacementProcessGeneration && ready.NewCredentialValidUntil.Equal(r.Install.NewCredentialValidUntil)
}

func (r Record) revokeMatches(revoke connectorprotocol.CredentialRotationRevoke) bool {
	sameReadySession := revoke.SessionID == r.Ready.SessionID && revoke.ProcessGeneration == r.Ready.ProcessGeneration
	reboundSession := revoke.SessionID != r.Ready.SessionID && revoke.ProcessGeneration > r.Ready.ProcessGeneration
	return revoke.AccountID == r.AccountID && revoke.TunnelID == r.TunnelID && revoke.OperationID == r.OperationID && revoke.ConnectorID == r.ConnectorID && revoke.HostID == r.HostID && revoke.TargetSetHash == r.TargetSetHash && (sameReadySession || reboundSession) && revoke.OldCredentialGeneration == r.OldCredentialGeneration && revoke.NewCredentialGeneration == r.NewCredentialGeneration
}

func (r Record) clone() Record {
	r.NewKey = r.NewKey.clone()
	if r.Challenge != nil {
		copy := *r.Challenge
		r.Challenge = &copy
	}
	if r.Install != nil {
		copy := *r.Install
		r.Install = &copy
	}
	if r.Ready != nil {
		copy := *r.Ready
		r.Ready = &copy
	}
	if r.Revoke != nil {
		copy := *r.Revoke
		r.Revoke = &copy
	}
	return r
}
