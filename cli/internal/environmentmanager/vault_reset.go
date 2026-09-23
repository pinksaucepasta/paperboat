package environmentmanager

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type VaultResetClient interface {
	GetVaultPersonalScopes(context.Context) (api.VaultPersonalInventory, error)
	ResetVault(context.Context, api.VaultReset) (api.PasswordVaultState, error)
}

// ResetPersonal is the explicitly destructive, normally authenticated total-loss
// path. It never decrypts or deletes another account's or a team's ciphertext.
func (v PasswordVault) ResetPersonal(ctx context.Context, password, recoveryCode []byte, confirmed bool) (resultErr error) {
	if !confirmed {
		return errors.New("personal ENV reset requires explicit total-loss confirmation")
	}
	unlock, err := v.lock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	c, ok := v.Client.(VaultResetClient)
	if !ok {
		return environmente2ee.ErrInvalid
	}
	local, err := v.Store.LoadPasswordVault(v.Issuer, v.AccountID)
	defer local.Clear()
	if err != nil && !errors.Is(err, config.ErrSecretNotFound) {
		return err
	}
	if local.Pending != nil || local.Operation != nil {
		return ErrVaultPending
	}
	head, raw, err := v.current(ctx)
	if err != nil {
		return err
	}
	if head.Generation < local.Head.Generation || head.Generation == local.Head.Generation && head.ID != local.Head.ID {
		return ErrAuthorityFork
	}
	inventory, err := c.GetVaultPersonalScopes(ctx)
	if err != nil {
		return err
	}
	if inventory.KeyEpoch == 0 || inventory.KeyEpoch >= environmente2ee.MaximumContractInteger {
		return ErrIntegrity
	}
	keys, err := environmente2ee.NewVaultKeys()
	if err != nil {
		return err
	}
	defer keys.Clear()
	keys.PersonalEpoch = inventory.KeyEpoch + 1
	oldProtection, err := environmente2ee.PasswordVaultProtection(raw)
	if err != nil {
		return ErrIntegrity
	}
	signer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	defer clear(signer)
	protection := environmente2ee.VaultProtection{WriterPublic: signer.Public().(ed25519.PublicKey), Password: environmente2ee.VaultCredential{Epoch: oldProtection.Password.Epoch}, RecoveryEpoch: oldProtection.RecoveryEpoch}
	protection, err = environmente2ee.ReplaceVaultPassword(ctx, head, protection, password)
	if err != nil {
		return err
	}
	if len(recoveryCode) > 0 {
		protection, err = environmente2ee.ReplaceVaultRecovery(ctx, head, protection, recoveryCode)
		if err != nil {
			return err
		}
	} else {
		protection.RecoveryEpoch++
		if protection.RecoveryEpoch > environmente2ee.MaximumContractInteger {
			return environmente2ee.ErrInvalid
		}
	}
	payload, err := keys.MarshalBinary()
	if err != nil {
		return err
	}
	defer clear(payload)
	envelope, next, err := environmente2ee.SealProtectedVault(ctx, environmente2ee.VaultHead{Issuer: v.Issuer, AccountID: v.AccountID, Generation: head.Generation + 1}, head.ID, protection, payload)
	if err != nil {
		return err
	}
	replacements := make([]string, 0, len(inventory.Scopes))
	seen := map[string]bool{}
	for _, state := range inventory.Scopes {
		if state.OwnerKind != "personal" || state.OwnerID != v.AccountID || state.KeyEpoch != inventory.KeyEpoch || seen[state.MachineID] || state.Revision == 0 || state.Revision >= environmente2ee.MaximumContractInteger {
			return ErrIntegrity
		}
		previous, err := environmente2ee.ParseDocumentID(state.DocumentID)
		if err != nil {
			return ErrIntegrity
		}
		seen[state.MachineID] = true
		scope, err := environmente2ee.SealVaultScope(ctx, environmente2ee.VaultScopeClaims{Issuer: v.Issuer, OwnerKind: "personal", OwnerID: v.AccountID, MachineID: state.MachineID, KeyEpoch: keys.PersonalEpoch, Revision: state.Revision + 1, Previous: previous[:], WriterAccount: v.AccountID, WriterVaultGeneration: next.Generation}, keys.PersonalKey, keys.WriterSeed, map[string][]byte{})
		if err != nil {
			return err
		}
		replacements = append(replacements, vaultEncoded(scope.Raw))
	}

	op, err := newVaultOperationID()
	if err != nil {
		return err
	}
	local.Clear()
	local = config.PasswordVaultRecord{Head: head, Envelope: raw, Pending: &config.PasswordVaultPending{Head: next, Envelope: envelope, Payload: payload}}
	return v.stageOperation(ctx, &local, "reset", "personal", v.AccountID, "", api.VaultReset{OperationID: op, ExpectedVaultDocumentID: head.ID.String(), VaultEnvelope: vaultEncoded(envelope), ScopeEnvelopes: replacements, ConfirmTotalLoss: true})
}

type VaultPersonalRotationClient interface {
	GetVaultPersonalScopes(context.Context) (api.VaultPersonalInventory, error)
	StageVaultPersonalScope(context.Context, string, api.VaultPersonalScopeStage) (api.VaultScopeDocument, error)
	RotateVaultPersonal(context.Context, api.VaultPersonalRotate) (api.PasswordVaultState, error)
	AbortVaultPersonalRotation(context.Context, string) error
}
type personalRotationJournal struct {
	Final     api.VaultPersonalRotate      `json:"final"`
	Inventory []api.VaultScopeState        `json:"inventory"`
	Upload    *api.VaultPersonalScopeStage `json:"upload,omitempty"`
}

// RotatePersonal retains one encrypted scope upload at a time, keeping memory,
// request and secure-store records bounded for the full 513-scope workload.
func (v PasswordVault) RotatePersonal(ctx context.Context) error {
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
		rotation, ok := v.Client.(VaultPersonalRotationClient)
		if !ok {
			return environmente2ee.ErrInvalid
		}
		inventory, err := rotation.GetVaultPersonalScopes(ctx)
		if err != nil {
			return err
		}
		if inventory.KeyEpoch != keys.PersonalEpoch || inventory.KeyEpoch >= environmente2ee.MaximumContractInteger || len(inventory.Scopes) > 513 {
			return ErrVaultChanged
		}
		seen := map[string]bool{}
		for i := range inventory.Scopes {
			state := &inventory.Scopes[i]
			if state.OwnerKind != "personal" || state.OwnerID != v.AccountID || state.KeyEpoch != inventory.KeyEpoch || seen[state.MachineID] || state.Revision == 0 {
				return ErrIntegrity
			}
			seen[state.MachineID] = true
			if _, err := environmente2ee.ParseDocumentID(state.DocumentID); err != nil {
				return ErrIntegrity
			}
			state.Envelope = "" // The journal needs heads, never copies of whole old scopes.
		}
		clear(keys.PersonalKey)
		keys.PersonalKey = make([]byte, 32)
		if _, err := rand.Read(keys.PersonalKey); err != nil {
			return err
		}
		keys.PersonalEpoch++
		if err := v.prepareKeySuccessor(ctx, local, keys); err != nil {
			return err
		}
		op, err := newVaultOperationID()
		if err != nil {
			return err
		}
		journal := personalRotationJournal{Final: api.VaultPersonalRotate{OperationID: op, ExpectedVaultDocumentID: local.Head.ID.String(), VaultEnvelope: vaultEncoded(local.Pending.Envelope), ScopeDocuments: []api.VaultScopeDocument{}}, Inventory: inventory.Scopes}
		return v.stageOperation(ctx, local, "personal-rotate", "personal", v.AccountID, "", journal)
	})
}
func (v PasswordVault) publishPersonalRotation(ctx context.Context, local *config.PasswordVaultRecord) error {
	rotation, ok := v.Client.(VaultPersonalRotationClient)
	if !ok || local.Operation == nil || local.Pending == nil {
		return ErrIntegrity
	}
	c, err := v.dataClient()
	if err != nil {
		return err
	}
	var journal personalRotationJournal
	if json.Unmarshal(local.Operation.Request, &journal) != nil || len(journal.Inventory) > 513 || len(journal.Final.ScopeDocuments) > len(journal.Inventory) || journal.Final.VaultEnvelope != vaultEncoded(local.Pending.Envelope) || journal.Final.ExpectedVaultDocumentID != local.Head.ID.String() {
		return ErrIntegrity
	}
	save := func() error {
		raw, err := json.Marshal(journal)
		if err != nil {
			return err
		}
		local.Operation.Request = raw
		return v.Store.SavePasswordVault(*local)
	}
	for len(journal.Final.ScopeDocuments) < len(journal.Inventory) {
		index := len(journal.Final.ScopeDocuments)
		state := journal.Inventory[index]
		if journal.Upload == nil {
			if len(local.Payload) == 0 || len(local.Pending.Payload) == 0 {
				return ErrVaultLocked
			}
			oldKeys, err := environmente2ee.ParseVaultKeys(local.Payload)
			if err != nil {
				return ErrIntegrity
			}
			nextKeys, err := environmente2ee.ParseVaultKeys(local.Pending.Payload)
			if err != nil {
				oldKeys.Clear()
				return ErrIntegrity
			}
			scope, values, err := v.readVaultScope(ctx, c, &oldKeys, "personal", v.AccountID, state.MachineID)
			oldKeys.Clear()
			if err != nil {
				nextKeys.Clear()
				return err
			}
			if scope.ID.String() != state.DocumentID || scope.Claims.Revision != state.Revision {
				clearVaultValues(values)
				nextKeys.Clear()
				return ErrVaultChanged
			}
			next, err := environmente2ee.SealVaultScope(ctx, environmente2ee.VaultScopeClaims{Issuer: v.Issuer, OwnerKind: "personal", OwnerID: v.AccountID, MachineID: state.MachineID, KeyEpoch: nextKeys.PersonalEpoch, Revision: scope.Claims.Revision + 1, Previous: scope.ID[:], WriterAccount: v.AccountID, WriterVaultGeneration: local.Pending.Head.Generation}, nextKeys.PersonalKey, nextKeys.WriterSeed, values)
			clearVaultValues(values)
			nextKeys.Clear()
			if err != nil {
				return err
			}
			journal.Upload = &api.VaultPersonalScopeStage{ExpectedVaultDocumentID: local.Head.ID.String(), MachineID: state.MachineID, Envelope: vaultEncoded(next.Raw)}
			if err := save(); err != nil {
				return err
			}
		}
		if journal.Upload.MachineID != state.MachineID || journal.Upload.ExpectedVaultDocumentID != local.Head.ID.String() {
			return ErrIntegrity
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(journal.Upload.Envelope)
		if err != nil {
			return ErrIntegrity
		}
		id := environmente2ee.DocumentID(sha256.Sum256(raw))
		out, err := rotation.StageVaultPersonalScope(ctx, journal.Final.OperationID, *journal.Upload)
		if err != nil {
			return err
		}
		if out.MachineID != state.MachineID || out.DocumentID != id.String() {
			return ErrIntegrity
		}
		journal.Final.ScopeDocuments = append(journal.Final.ScopeDocuments, out)
		journal.Upload = nil
		if err := save(); err != nil {
			return err
		}
	}
	out, err := rotation.RotateVaultPersonal(ctx, journal.Final)
	if err != nil {
		return err
	}
	if out.Envelope != journal.Final.VaultEnvelope {
		return ErrIntegrity
	}
	return nil
}

// AbortPersonalRotation abandons only uncommitted encrypted staging. It never
// rolls back an already committed vault; exact resume is required in that case.
func (v PasswordVault) AbortPersonalRotation(ctx context.Context) (resultErr error) {
	unlock, err := v.lock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	local, err := v.Store.LoadPasswordVault(v.Issuer, v.AccountID)
	if err != nil {
		return err
	}
	defer local.Clear()
	if local.Operation == nil || local.Operation.Kind != "personal-rotate" {
		return ErrVaultPending
	}
	head, _, err := v.current(ctx)
	if err != nil {
		return err
	}
	if head != local.Head {
		return ErrVaultChanged
	}
	var journal personalRotationJournal
	if json.Unmarshal(local.Operation.Request, &journal) != nil {
		return ErrIntegrity
	}
	c, ok := v.Client.(VaultPersonalRotationClient)
	if !ok {
		return ErrIntegrity
	}
	if err := c.AbortVaultPersonalRotation(ctx, journal.Final.OperationID); err != nil {
		return err
	}
	if local.Pending != nil {
		clear(local.Pending.Payload)
	}
	local.Pending = nil
	local.Operation = nil
	return v.Store.SavePasswordVault(local)
}
