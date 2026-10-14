package environmentmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
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
	records    map[string]api.VaultRecordsResult
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
	if local.Pending == nil || local.Operation == nil || json.Unmarshal(local.Operation.Request, &journal) != nil || journal.Upload == nil || !reflect.DeepEqual(*journal.Upload, in) || journal.Final.OperationID != op {
		return api.VaultScopeDocument{}, errors.New("upload not exactly staged")
	}
	if prior, ok := c.stages[in.MachineID]; ok && !reflect.DeepEqual(prior, in) {
		return api.VaultScopeDocument{}, errors.New("changed staged ciphertext")
	}
	current := c.records[scopeControlKey("personal", c.account, in.MachineID)]
	liveIDs := []string{}
	for _, record := range current.Records {
		if !record.Deleted {
			liveIDs = append(liveIDs, record.DocumentID)
		}
	}
	sort.Strings(liveIDs)
	if in.Records.ExpectedSequence != current.Sequence || !reflect.DeepEqual(in.Records.DocumentIDs, liveIDs) || len(in.Records.Envelopes) != len(liveIDs) {
		return api.VaultScopeDocument{}, errors.New("replacement not fenced to full live record inventory")
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
	return api.VaultScopeDocument{WorkspaceID: in.WorkspaceID, MachineID: in.MachineID, DocumentID: scope.DocumentID}, nil
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
		result := api.VaultRecordsResult{Sequence: 1, Records: []api.VaultRecordState{}}
		for _, envelope := range stage.Records.Envelopes {
			state, err := rotationRecordState(envelope, c.writerPublic, 1)
			if err != nil {
				return api.PasswordVaultState{}, err
			}
			result.Records = append(result.Records, state)
		}
		c.records[scopeControlKey("personal", c.account, ref.MachineID)] = result
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
	control := &personalRotationControl{vaultScopesControl: base, stages: map[string]api.VaultPersonalScopeStage{}, epoch: 1, failStage: true, failFinal: true, records: map[string]api.VaultRecordsResult{}}
	v.Client = control
	for _, machine := range []string{"", "machine_1"} {
		if err := v.MutateScope(ctx, "personal", v.AccountID, machine, func(values map[string][]byte) error { values["PRESERVED"] = []byte("rotation-test-value"); return nil }); err != nil {
			t.Fatal(err)
		}
		if err := v.SetScopeVariable(ctx, "personal", v.AccountID, machine, "REMOVED", []byte("discarded-value")); err != nil {
			t.Fatal(err)
		}
		if err := v.RemoveScopeVariable(ctx, "personal", v.AccountID, machine, "REMOVED"); err != nil {
			t.Fatal(err)
		}
	}
	before, oldKeys := loadVaultKeysForScopeTest(t, store)
	defer before.Clear()
	defer oldKeys.Clear()

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
	if len(control.stageCalls) != 3 || !reflect.DeepEqual(control.stageCalls[0], control.stageCalls[1]) {
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
		anchor, err := environmente2ee.OpenVaultScope(ctx, scope, nextKeys.PersonalKey)
		if err != nil || len(anchor) != 0 {
			t.Fatal("rotated anchor contains values", err)
		}
		clearVaultValues(anchor)
		newRecordKey, err := v.recordKey(&nextKeys, scope)
		if err != nil {
			t.Fatal(err)
		}
		oldScope := scope
		oldScope.Claims.KeyEpoch = oldKeys.PersonalEpoch
		oldRecordKey, err := v.recordKey(&oldKeys, oldScope)
		if err != nil {
			t.Fatal(err)
		}
		records := control.records[scopeControlKey("personal", v.AccountID, state.MachineID)]
		if len(records.Records) != 1 {
			t.Fatal("rotation lost live record inventory")
		}
		for _, recordState := range records.Records {
			record, err := recordState.Decode()
			if err != nil {
				t.Fatal(err)
			}
			name, value, err := environmente2ee.OpenVaultRecord(ctx, record, newRecordKey)
			if err != nil || name != "PRESERVED" || string(value) != "rotation-test-value" || record.Claims.Revision != 1 || record.Claims.WriterVaultGeneration != after.Head.Generation {
				clear(value)
				t.Fatal("rotation lost value or epoch metadata", err)
			}
			clear(value)
			if _, value, err := environmente2ee.OpenVaultRecord(ctx, record, oldRecordKey); err == nil {
				clear(value)
				t.Fatal("old scope key decrypts rotated record")
			}
		}
		clear(newRecordKey)
		clear(oldRecordKey)
	}
}

func rotationRecordState(envelope string, writer []byte, sequence uint64) (api.VaultRecordState, error) {
	raw, err := decodeVaultTestEnvelope(envelope)
	if err != nil {
		return api.VaultRecordState{}, err
	}
	record, err := environmente2ee.ParseVaultRecord(raw, writer)
	if err != nil {
		return api.VaultRecordState{}, err
	}
	c := record.Claims
	return api.VaultRecordState{WorkspaceID: c.WorkspaceID, OwnerKind: c.OwnerKind, OwnerID: c.OwnerID, MachineID: c.MachineID, KeyEpoch: c.KeyEpoch, RecordID: c.RecordID, Revision: c.Revision, Deleted: c.Deleted, Sequence: sequence, DocumentID: record.ID.String(), WriterPublic: base64.RawURLEncoding.EncodeToString(writer), Envelope: envelope}, nil
}
func (c *personalRotationControl) VaultRecords(_ context.Context, coordinate api.VaultLayerCoordinate, _ uint64) (api.VaultRecordsResult, error) {
	out := c.records[scopeControlKey(coordinate.OwnerKind, coordinate.OwnerID, coordinate.MachineID)]
	out.Records = append([]api.VaultRecordState{}, out.Records...)
	return out, nil
}
func (c *personalRotationControl) GetVaultRecord(ctx context.Context, coordinate api.VaultLayerCoordinate, id string) (api.VaultRecordState, error) {
	result, _ := c.VaultRecords(ctx, coordinate, 0)
	for _, record := range result.Records {
		if record.RecordID == id {
			return record, nil
		}
	}
	return api.VaultRecordState{}, &api.APIError{Status: 404}
}
func (c *personalRotationControl) PutVaultRecords(_ context.Context, coordinate api.VaultLayerCoordinate, in api.VaultRecordsPut) (api.VaultRecordsResult, error) {
	key := scopeControlKey(coordinate.OwnerKind, coordinate.OwnerID, coordinate.MachineID)
	result := c.records[key]
	result.Sequence++
	for _, mutation := range in.Records {
		state, err := rotationRecordState(mutation.Envelope, c.writerPublic, result.Sequence)
		if err != nil {
			return api.VaultRecordsResult{}, err
		}
		index := -1
		for i, old := range result.Records {
			if old.RecordID == state.RecordID {
				index = i
				if old.Revision != mutation.ExpectedRevision {
					return api.VaultRecordsResult{}, errors.New("record CAS conflict")
				}
			}
		}
		if index < 0 && mutation.ExpectedRevision != 0 {
			return api.VaultRecordsResult{}, errors.New("record CAS conflict")
		}
		if index < 0 {
			result.Records = append(result.Records, state)
		} else {
			result.Records[index] = state
		}
	}
	c.records[key] = result
	out := api.VaultRecordsResult{Sequence: result.Sequence}
	for _, mutation := range in.Records {
		state, _ := rotationRecordState(mutation.Envelope, c.writerPublic, result.Sequence)
		out.Records = append(out.Records, state)
	}
	return out, nil
}
