package environmentmanager

import (
	"bytes"
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

var ErrVaultChanged = errors.New("ENV vault changed; unlock the current vault before retrying")
var ErrVaultLocked = errors.New("ENV vault is locked; run `pb env vault unlock` before changing ENV keys or values")
var ErrVaultPending = errors.New("ENV vault publication is pending; run `pb env vault resume` before retrying")

type PasswordVaultClient interface {
	GetPasswordVault(context.Context) (api.PasswordVaultState, error)
	PutPasswordVault(context.Context, []byte) (api.PasswordVaultState, error)
}

// PasswordVault owns local custody and publication reconciliation. None of these
// operations authorizes enrollment, team membership, or host execution.
type PasswordVault struct {
	Client    PasswordVaultClient
	Store     config.ProfileStore
	Issuer    string
	AccountID string
}

func (v PasswordVault) lock() (func() error, error) {
	if v.Client == nil || v.Issuer == "" || v.AccountID == "" {
		return nil, environmente2ee.ErrInvalid
	}
	return v.Store.LockEnvironmentMutations(v.Issuer, v.AccountID, "password_vault")
}

func (v PasswordVault) current(ctx context.Context) (environmente2ee.VaultHead, []byte, error) {
	state, err := v.Client.GetPasswordVault(ctx)
	if err != nil {
		return environmente2ee.VaultHead{}, nil, err
	}
	head, raw, err := state.Decode()
	if err != nil || head.Issuer != v.Issuer || head.AccountID != v.AccountID {
		return environmente2ee.VaultHead{}, nil, ErrIntegrity
	}
	return head, raw, nil
}

func (v PasswordVault) Initialize(ctx context.Context, password []byte) error {
	return v.InitializeWithRecovery(ctx, password, nil)
}

// InitializeWithRecovery accepts a code already displayed/saved by the caller.
func (v PasswordVault) InitializeWithRecovery(ctx context.Context, password, recoveryCode []byte) (resultErr error) {
	unlock, err := v.lock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	local, err := v.Store.LoadPasswordVault(v.Issuer, v.AccountID)
	defer local.Clear()
	if err == nil {
		return ErrVaultChanged
	}
	if !errors.Is(err, config.ErrSecretNotFound) {
		return err
	}
	if _, _, err := v.current(ctx); !api.IsNotFound(err) {
		if err == nil {
			return ErrVaultChanged
		}
		return err
	}
	keys, err := environmente2ee.NewVaultKeys()
	if err != nil {
		return err
	}
	defer keys.Clear()
	payload, err := keys.MarshalBinary()
	if err != nil {
		return err
	}
	defer clear(payload)
	initial := environmente2ee.VaultHead{Issuer: v.Issuer, AccountID: v.AccountID, Generation: 1}
	protection, err := environmente2ee.NewVaultProtection(ctx, initial, password, recoveryCode, keys.WriterSeed)
	if err != nil {
		return err
	}
	raw, head, err := environmente2ee.SealProtectedVault(ctx, initial, environmente2ee.DocumentID{}, protection, payload)
	if err != nil {
		return err
	}
	local.Pending = &config.PasswordVaultPending{Head: head, Envelope: raw, Payload: payload}
	if err := v.Store.SavePasswordVault(local); err != nil {
		return err
	}
	return v.publish(ctx, &local)
}

func (v PasswordVault) Unlock(ctx context.Context, password []byte) (resultErr error) {
	unlock, err := v.lock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	local, err := v.Store.LoadPasswordVault(v.Issuer, v.AccountID)
	defer local.Clear()
	if err != nil && !errors.Is(err, config.ErrSecretNotFound) {
		return err
	}
	if local.Operation != nil && local.Operation.Kind == "personal-rotate" && local.Pending != nil {
		head, _, err := v.current(ctx)
		if err != nil {
			return err
		}
		if head != local.Head && head != local.Pending.Head {
			return ErrVaultChanged
		}
		nextPayload, err := environmente2ee.OpenPasswordVault(ctx, local.Pending.Head, password, local.Pending.Envelope)
		if err != nil {
			return err
		}
		defer clear(nextPayload)
		var previousPayload []byte
		if head == local.Head {
			previousPayload, err = environmente2ee.OpenPasswordVault(ctx, local.Head, password, local.Envelope)
			if err != nil {
				return err
			}
			defer clear(previousPayload)
		}
		local.Clear()
		local.Payload, local.Pending.Payload = previousPayload, nextPayload
		return v.Store.SavePasswordVault(local)
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
	payload, err := environmente2ee.OpenPasswordVault(ctx, head, password, raw)
	if err != nil {
		return err
	}
	defer clear(payload)
	keys, err := environmente2ee.ParseVaultKeys(payload)
	if err != nil {
		return ErrIntegrity
	}
	keys.Clear()
	return v.Store.SavePasswordVault(config.PasswordVaultRecord{Head: head, Envelope: raw, Payload: payload})
}

// ChangePassword also supports surviving-unlocked-device recovery: the old
// password is not needed when usable keys remain in authorized secure custody.
func (v PasswordVault) ChangePassword(ctx context.Context, password []byte) (resultErr error) {
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
	if local.Pending != nil || local.Operation != nil {
		return ErrVaultPending
	}
	if len(local.Payload) == 0 {
		return ErrVaultLocked
	}
	head, _, err := v.current(ctx)
	if err != nil {
		return err
	}
	if head != local.Head {
		return ErrVaultChanged
	}
	keys, err := environmente2ee.ParseVaultKeys(local.Payload)
	if err != nil {
		return ErrIntegrity
	}
	keys.Clear()
	protection, err := environmente2ee.PasswordVaultProtection(local.Envelope)
	if err != nil {
		return ErrIntegrity
	}
	protection, err = environmente2ee.ReplaceVaultPassword(ctx, head, protection, password)
	if err != nil {
		return err
	}
	raw, next, err := environmente2ee.SealProtectedVault(ctx, environmente2ee.VaultHead{Issuer: v.Issuer, AccountID: v.AccountID, Generation: head.Generation + 1}, head.ID, protection, local.Payload)
	if err != nil {
		return err
	}
	local.Pending = &config.PasswordVaultPending{Head: next, Envelope: raw, Payload: local.Payload}
	if err := v.Store.SavePasswordVault(local); err != nil {
		return err
	}
	return v.publish(ctx, &local)
}

func (v PasswordVault) Resume(ctx context.Context) (resultErr error) {
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
	if local.Operation != nil {
		return v.publishOperation(ctx, &local)
	}
	if local.Pending == nil {
		return nil
	}
	return v.publish(ctx, &local)
}

func (v PasswordVault) publish(ctx context.Context, local *config.PasswordVaultRecord) error {
	pending := local.Pending
	if pending == nil {
		return environmente2ee.ErrInvalid
	}
	state, err := v.Client.PutPasswordVault(ctx, pending.Envelope)
	if err != nil {
		return err
	} // Keep exact pending bytes for an idempotent retry.
	head, _, err := state.Decode()
	if err != nil || head != pending.Head {
		return ErrIntegrity
	}
	next := config.PasswordVaultRecord{Head: pending.Head, Envelope: pending.Envelope, Payload: bytes.Clone(pending.Payload)}
	if err := v.Store.SavePasswordVault(next); err != nil {
		next.Clear()
		return err
	}
	local.Clear()
	local.Head, local.Envelope, local.Payload, local.Pending = next.Head, next.Envelope, next.Payload, nil
	return nil
}

// ReplaceRecovery enables/replaces a previously displayed code, or disables with nil.
// Neither the old code nor the master password is required while unlocked.
func (v PasswordVault) ReplaceRecovery(ctx context.Context, code []byte) (resultErr error) {
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
	if local.Pending != nil || local.Operation != nil {
		return ErrVaultPending
	}
	if len(local.Payload) == 0 {
		return ErrVaultLocked
	}
	head, _, err := v.current(ctx)
	if err != nil {
		return err
	}
	if head != local.Head {
		return ErrVaultChanged
	}
	protection, err := environmente2ee.PasswordVaultProtection(local.Envelope)
	if err != nil {
		return ErrIntegrity
	}
	protection, err = environmente2ee.ReplaceVaultRecovery(ctx, head, protection, code)
	if err != nil {
		return err
	}
	return v.stageVault(ctx, &local, protection, local.Payload)
}

// Recover never restores device credentials or grants membership. The caller must
// display and acknowledge the replacement code before calling this operation.
func (v PasswordVault) Recover(ctx context.Context, code, password, replacementCode []byte) (resultErr error) {
	if len(replacementCode) == 0 {
		return environmente2ee.ErrInvalid
	}
	unlock, err := v.lock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
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
	payload, err := environmente2ee.OpenRecoveryVault(ctx, head, code, raw)
	if err != nil {
		return err
	}
	defer clear(payload)
	protection, err := environmente2ee.PasswordVaultProtection(raw)
	if err != nil {
		return ErrIntegrity
	}
	protection, err = environmente2ee.ReplaceVaultPassword(ctx, head, protection, password)
	if err != nil {
		return err
	}
	protection, err = environmente2ee.ReplaceVaultRecovery(ctx, head, protection, replacementCode)
	if err != nil {
		return err
	}
	local.Clear()
	local = config.PasswordVaultRecord{Head: head, Envelope: raw, Payload: payload}
	return v.stageVault(ctx, &local, protection, payload)
}

// UpdateKeys keeps both recovery and password access current without re-entering
// either credential. Authorization of the actual team/scope mutation is separate.
func (v PasswordVault) UpdateKeys(ctx context.Context, mutate func(*environmente2ee.VaultKeys) error) (resultErr error) {
	if mutate == nil {
		return environmente2ee.ErrInvalid
	}
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
	if local.Pending != nil || local.Operation != nil {
		return ErrVaultPending
	}
	if len(local.Payload) == 0 {
		return ErrVaultLocked
	}
	head, _, err := v.current(ctx)
	if err != nil {
		return err
	}
	if head != local.Head {
		return ErrVaultChanged
	}
	keys, err := environmente2ee.ParseVaultKeys(local.Payload)
	if err != nil {
		return ErrIntegrity
	}
	defer keys.Clear()
	if err := mutate(&keys); err != nil {
		return err
	}
	payload, err := keys.MarshalBinary()
	if err != nil {
		return err
	}
	defer clear(payload)
	protection, err := environmente2ee.PasswordVaultProtection(local.Envelope)
	if err != nil {
		return ErrIntegrity
	}
	return v.stageVault(ctx, &local, protection, payload)
}
func (v PasswordVault) stageVault(ctx context.Context, local *config.PasswordVaultRecord, protection environmente2ee.VaultProtection, payload []byte) error {
	head := local.Head
	raw, next, err := environmente2ee.SealProtectedVault(ctx, environmente2ee.VaultHead{Issuer: v.Issuer, AccountID: v.AccountID, Generation: head.Generation + 1}, head.ID, protection, payload)
	if err != nil {
		return err
	}
	local.Pending = &config.PasswordVaultPending{Head: next, Envelope: raw, Payload: payload}
	if err := v.Store.SavePasswordVault(*local); err != nil {
		return err
	}
	return v.publish(ctx, local)
}
