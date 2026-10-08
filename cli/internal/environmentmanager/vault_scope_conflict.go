package environmentmanager

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type ScopeRefreshConflict struct {
	Cause        error
	EpochChanged bool
}

func (e *ScopeRefreshConflict) Error() string {
	if e.EpochChanged {
		return "ENV source key epoch changed; unlock current keys and submit the intended edit again"
	}
	return "ENV source changed; read the current source and submit the intended edit again"
}
func (e *ScopeRefreshConflict) Unwrap() error { return e.Cause }

type ScopePublicationPending struct{ Cause error }

func (e *ScopePublicationPending) Error() string {
	return "ENV source publication remains pending; resume the exact staged operation"
}
func (e *ScopePublicationPending) Unwrap() error { return e.Cause }

func (v PasswordVault) reconcileScopeConflict(ctx context.Context, local *config.PasswordVaultRecord, c VaultDataClient, in api.VaultScopePut, rejected error) error {
	pending := func(cause error) error { return &ScopePublicationPending{Cause: errors.Join(rejected, cause)} }
	op := local.Operation
	if op == nil || op.Kind != "scope-put" || local.Pending != nil {
		return pending(ErrIntegrity)
	}
	currentState, err := c.GetVaultScope(ctx, op.OwnerKind, op.OwnerID, op.MachineID)
	if err != nil {
		return pending(err)
	}
	current, err := currentState.Decode()
	if err != nil {
		return pending(ErrIntegrity)
	}
	var writer []byte
	if len(local.Payload) > 0 {
		keys, err := environmente2ee.ParseVaultKeys(local.Payload)
		if err != nil {
			return pending(ErrIntegrity)
		}
		signing := ed25519.NewKeyFromSeed(keys.WriterSeed)
		writer = append([]byte(nil), signing.Public().(ed25519.PublicKey)...)
		clear(signing)
		keys.Clear()
	} else {
		sharing, err := c.GetVaultSharing(ctx, v.AccountID)
		if err != nil {
			return pending(err)
		}
		if sharing.AccountID != v.AccountID || sharing.VaultGeneration < local.Head.Generation {
			return pending(ErrIntegrity)
		}
		writer, err = base64.RawURLEncoding.Strict().DecodeString(sharing.WriterPublic)
		if err != nil {
			return pending(ErrIntegrity)
		}
	}
	if len(in.Envelope) > base64.RawURLEncoding.EncodedLen(environmente2ee.MaximumVaultScopeBytes) {
		return pending(ErrIntegrity)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(in.Envelope)
	if err != nil {
		return pending(ErrIntegrity)
	}
	candidate, err := environmente2ee.ParseVaultScope(raw, writer)
	if err != nil {
		return pending(ErrIntegrity)
	}
	expected := candidate.Claims
	sameTarget := func(scope environmente2ee.VaultScope) bool {
		claims := scope.Claims
		return claims.Issuer == v.Issuer && claims.OwnerKind == op.OwnerKind && claims.OwnerID == op.OwnerID && claims.MachineID == op.MachineID
	}
	if !sameTarget(candidate) || !sameTarget(current) || expected.WriterAccount != v.AccountID || expected.WriterVaultGeneration != local.Head.Generation {
		return pending(ErrIntegrity)
	}
	if current.Claims.KeyEpoch < expected.KeyEpoch || current.Claims.KeyEpoch == expected.KeyEpoch && current.Claims.Revision < expected.Revision {
		return pending(ErrIntegrity)
	}
	committed := current.ID == candidate.ID
	epochChanged := current.Claims.KeyEpoch > expected.KeyEpoch
	// The authorized signed current cursor proves the candidate is committed or
	// superseded. Preserve custody, floors, and every key successor unchanged.
	local.Operation = nil
	if err := v.Store.SavePasswordVault(*local); err != nil {
		return pending(err)
	}
	if committed {
		return nil
	}
	return &ScopeRefreshConflict{Cause: rejected, EpochChanged: epochChanged}
}
