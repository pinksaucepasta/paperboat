package environmentmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type personalRotationControl struct {
	*vaultScopesControl
	stages     map[string]api.VaultPersonalScopeStage
	stageCalls []api.VaultPersonalScopeStage
	finalCalls []api.VaultPersonalRotate
	failStage  bool
	failFinal  bool
	epoch      uint64
}

func (c *personalRotationControl) GetVaultPersonalScopes(context.Context) (api.VaultPersonalInventory, error) {
	out := api.VaultPersonalInventory{KeyEpoch: c.epoch, Scopes: []api.VaultScopeState{}}
	for _, scope := range c.scopes {
		if scope.OwnerKind == "personal" {
			scope.Envelope = ""
			out.Scopes = append(out.Scopes, scope)
		}
	}
	sort.Slice(out.Scopes, func(i, j int) bool { return out.Scopes[i].MachineID < out.Scopes[j].MachineID })
	return out, nil
}
func (c *personalRotationControl) StageVaultPersonalScope(_ context.Context, op string, in api.VaultPersonalScopeStage) (api.VaultScopeDocument, error) {
	local, err := c.store.LoadPasswordVault(c.issuer, c.account)
	if err != nil {
		return api.VaultScopeDocument{}, err
	}
	defer local.Clear()
	var journal personalRotationJournal
	if local.Pending == nil || local.Operation == nil || json.Unmarshal(local.Operation.Request, &journal) != nil || journal.Upload == nil || *journal.Upload != in || journal.Final.OperationID != op {
		return api.VaultScopeDocument{}, errors.New("upload not exactly staged")
	}
	if prior, ok := c.stages[in.MachineID]; ok && prior != in {
		return api.VaultScopeDocument{}, errors.New("changed staged ciphertext")
	}
	c.stageCalls = append(c.stageCalls, in)
	c.stages[in.MachineID] = in
	scope, err := c.scopeState("personal", c.account, in.MachineID, in.Envelope)
	if err != nil {
		return api.VaultScopeDocument{}, err
	}
	if c.failStage {
		c.failStage = false
		return api.VaultScopeDocument{}, errors.New("lost stage response")
	}
	return api.VaultScopeDocument{MachineID: in.MachineID, DocumentID: scope.DocumentID}, nil
}
func (c *personalRotationControl) RotateVaultPersonal(_ context.Context, in api.VaultPersonalRotate) (api.PasswordVaultState, error) {
	c.finalCalls = append(c.finalCalls, in)
	if c.state.Envelope == in.VaultEnvelope {
		return c.state, nil
	}
	if in.ExpectedVaultDocumentID != c.state.DocumentID || len(in.ScopeDocuments) != len(c.stages) {
		return api.PasswordVaultState{}, errors.New("incorrect full inventory")
	}
	for _, ref := range in.ScopeDocuments {
		stage, ok := c.stages[ref.MachineID]
		if !ok {
			return api.PasswordVaultState{}, errors.New("unstaged scope")
		}
		scope, err := c.scopeState("personal", c.account, ref.MachineID, stage.Envelope)
		if err != nil || scope.DocumentID != ref.DocumentID {
			return api.PasswordVaultState{}, errors.New("wrong scope digest")
		}
		c.scopes[scopeControlKey("personal", c.account, ref.MachineID)] = scope
	}
	raw, err := decodeVaultTestEnvelope(in.VaultEnvelope)
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	c.state, err = passwordVaultState(raw)
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	c.epoch++
	if c.failFinal {
		c.failFinal = false
		return api.PasswordVaultState{}, errors.New("lost final response")
	}
	return c.state, nil
}
func (c *personalRotationControl) AbortVaultPersonalRotation(context.Context, string) error {
	c.stages = map[string]api.VaultPersonalScopeStage{}
	return nil
}

func TestPersonalRotationResumesBoundedUploadsAfterLockAndLostCommit(t *testing.T) {
	ctx := context.Background()
	v, base, store := newVaultScopesFixture(t)
	for _, machine := range []string{"", "machine_1"} {
		if err := v.MutateScope(ctx, "personal", v.AccountID, machine, func(values map[string][]byte) error { values["PRESERVED"] = []byte("rotation-test-value"); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	before, oldKeys := loadVaultKeysForScopeTest(t, store)
	defer before.Clear()
	defer oldKeys.Clear()
	control := &personalRotationControl{vaultScopesControl: base, stages: map[string]api.VaultPersonalScopeStage{}, epoch: 1, failStage: true, failFinal: true}
	v.Client = control
	if err := v.RotatePersonal(ctx); err == nil {
		t.Fatal("lost upload response hidden")
	}
	if control.state.Generation != before.Head.Generation {
		t.Fatal("partial upload changed authoritative vault")
	}
	pending, err := store.LoadPasswordVault(v.Issuer, v.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	pendingID := pending.Pending.Head.ID
	pending.Clear()
	if err := store.LockPasswordVault(v.Issuer, v.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := v.Resume(ctx); !errors.Is(err, ErrVaultLocked) {
		t.Fatal("unfinished locked rotation must require keys", err)
	}
	if err := v.Unlock(ctx, []byte("test master password")); err != nil {
		t.Fatal(err)
	}
	if err := v.Resume(ctx); err == nil {
		t.Fatal("lost final response hidden")
	}
	if err := v.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	after, nextKeys := loadVaultKeysForScopeTest(t, store)
	defer after.Clear()
	defer nextKeys.Clear()
	if after.Pending != nil || after.Operation != nil || after.Head.ID != pendingID || nextKeys.PersonalEpoch != 2 || bytes.Equal(nextKeys.PersonalKey, oldKeys.PersonalKey) {
		t.Fatal("rotation did not commit exact successor and fresh epoch")
	}
	if len(control.stageCalls) != 3 || control.stageCalls[0] != control.stageCalls[1] {
		t.Fatal("stage retry changed encrypted bytes")
	}
	if len(control.finalCalls) != 2 {
		t.Fatal("final commit not retried exactly")
	}
	a, _ := json.Marshal(control.finalCalls[0])
	b, _ := json.Marshal(control.finalCalls[1])
	if !bytes.Equal(a, b) {
		t.Fatal("final retry changed request")
	}
	for _, state := range control.scopes {
		scope, err := state.Decode()
		if err != nil {
			t.Fatal(err)
		}
		values, err := environmente2ee.OpenVaultScope(ctx, scope, nextKeys.PersonalKey)
		if err != nil || string(values["PRESERVED"]) != "rotation-test-value" {
			t.Fatal("rotation lost a value", err)
		}
		clearVaultValues(values)
		if values, err := environmente2ee.OpenVaultScope(ctx, scope, oldKeys.PersonalKey); err == nil {
			clearVaultValues(values)
			t.Fatal("old epoch decrypts rotated scope")
		}
	}
}
