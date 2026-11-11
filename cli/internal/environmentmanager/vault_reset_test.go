package environmentmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type resetControl struct {
	*personalRotationControl
	resets int
}

func (c *resetControl) ResetVault(_ context.Context, in api.VaultReset) (api.PasswordVaultState, error) {
	if !in.ConfirmTotalLoss || in.ExpectedVaultDocumentID != c.state.DocumentID {
		return api.PasswordVaultState{}, errors.New("reset not fenced to the current vault")
	}
	raw, err := decodeVaultTestEnvelope(in.VaultEnvelope)
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	protection, err := environmente2ee.PasswordVaultProtection(raw)
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	inventory, _ := c.GetVaultPersonalScopes(context.Background())
	if len(in.ScopeEnvelopes) != len(inventory.Scopes) {
		return api.PasswordVaultState{}, errors.New("reset does not replace every personal scope")
	}
	c.writerPublic = protection.WriterPublic
	for i, state := range inventory.Scopes {
		scope, err := c.scopeState("personal", c.account, state.MachineID, in.ScopeEnvelopes[i])
		if err != nil {
			return api.PasswordVaultState{}, err
		}
		key := scopeControlKey("personal", c.account, state.MachineID)
		c.scopes[key] = scope
		delete(c.records, key) // a reset deletes every personal value
		delete(c.vaultScopesControl.records, key)
	}
	if c.state, err = passwordVaultState(raw); err != nil {
		return api.PasswordVaultState{}, err
	}
	c.epoch++
	c.resets++
	return c.state, nil
}

// The documented path for a user who lost both the password and the recovery code: reset works on a locked vault
// (no old keys anywhere), deletes every personal value, and the new password unlocks the replacement keys.
func TestPersonalResetWorksOnALockedVault(t *testing.T) {
	ctx := context.Background()
	v, base, store := newVaultScopesFixture(t)
	control := &resetControl{personalRotationControl: &personalRotationControl{vaultScopesControl: base, stages: map[string]api.VaultPersonalScopeStage{}, epoch: 1, records: map[string]api.VaultRecordsResult{}}}
	v.Client = control
	if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(values map[string][]byte) error { values["LOST"] = []byte("gone-after-reset"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.LockPasswordVault(v.Issuer, v.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := v.ResetPersonal(ctx, []byte("a new master password"), nil, false); err == nil {
		t.Fatal("reset without the total-loss confirmation ran")
	}
	if err := v.ResetPersonal(ctx, []byte("a new master password"), nil, true); err != nil {
		t.Fatalf("reset of a locked vault refused: %v", err)
	}
	if control.resets != 1 {
		t.Fatalf("resets=%d", control.resets)
	}
	if err := store.LockPasswordVault(v.Issuer, v.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(ctx, []byte("test master password")); err == nil {
		t.Fatal("the forgotten password still unlocks the reset vault")
	}
	if err := v.Unlock(ctx, []byte("a new master password")); err != nil {
		t.Fatalf("the new password does not unlock the reset vault: %v", err)
	}
	names, err := v.ListScopeNames(ctx, "personal", v.AccountID, "")
	if err != nil || len(names) != 0 {
		t.Fatalf("personal values survived the reset: %v %v", names, err)
	}
}
