package environmentmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

func recordControlKey(c api.VaultLayerCoordinate) string {
	return c.WorkspaceID + "\x00" + scopeControlKey(c.OwnerKind, c.OwnerID, c.MachineID)
}
func (c *vaultScopesControl) VaultRecords(_ context.Context, coordinate api.VaultLayerCoordinate, _ uint64) (api.VaultRecordsResult, error) {
	c.recordSnapshots++
	out := c.records[recordControlKey(coordinate)]
	out.Records = append([]api.VaultRecordState{}, out.Records...)
	return out, nil
}
func (c *vaultScopesControl) GetVaultRecord(_ context.Context, coordinate api.VaultLayerCoordinate, id string) (api.VaultRecordState, error) {
	c.recordGets++
	for _, record := range c.records[recordControlKey(coordinate)].Records {
		if record.RecordID == id {
			return record, nil
		}
	}
	return api.VaultRecordState{}, &api.APIError{Status: 404}
}
func (c *vaultScopesControl) PutVaultRecords(_ context.Context, coordinate api.VaultLayerCoordinate, in api.VaultRecordsPut) (api.VaultRecordsResult, error) {
	local, err := c.store.LoadPasswordVault(c.issuer, c.account)
	if err != nil {
		return api.VaultRecordsResult{}, err
	}
	defer local.Clear()
	var staged api.VaultRecordsPut
	if local.Operation == nil || local.Operation.Kind != "records-put" || local.Operation.WorkspaceID != coordinate.WorkspaceID || local.Operation.OwnerKind != coordinate.OwnerKind || local.Operation.OwnerID != coordinate.OwnerID || local.Operation.MachineID != coordinate.MachineID || json.Unmarshal(local.Operation.Request, &staged) != nil || !reflect.DeepEqual(staged, in) || bytes.Contains(local.Operation.Request, c.canary) && len(c.canary) > 0 {
		return api.VaultRecordsResult{}, errors.New("record operation not exactly encrypted and journaled")
	}
	c.recordPuts = append(c.recordPuts, in)
	if c.recordFailure == "conflict" {
		return api.VaultRecordsResult{}, &api.APIError{Status: 409, Code: "version_conflict"}
	}
	if previous, ok := c.recordOperations[in.OperationID]; ok {
		if !reflect.DeepEqual(c.recordOperationRequests[in.OperationID], in) {
			return api.VaultRecordsResult{}, errors.New("changed record replay")
		}
		return previous, nil
	}
	key := recordControlKey(coordinate)
	current := c.records[key]
	next := api.VaultRecordsResult{Sequence: current.Sequence + 1, Records: append([]api.VaultRecordState{}, current.Records...)}
	out := api.VaultRecordsResult{Sequence: next.Sequence}
	for _, mutation := range in.Records {
		state, err := rotationRecordState(mutation.Envelope, c.writerPublic, next.Sequence)
		if err != nil {
			return out, err
		}
		if state.WorkspaceID != coordinate.WorkspaceID || state.OwnerKind != coordinate.OwnerKind || state.OwnerID != coordinate.OwnerID || state.MachineID != coordinate.MachineID || state.Revision != mutation.ExpectedRevision+1 {
			return out, errors.New("wrong signed record coordinate or revision")
		}
		index := -1
		for i, old := range next.Records {
			if old.RecordID == state.RecordID {
				index = i
				if old.Revision != mutation.ExpectedRevision {
					return out, &api.APIError{Status: 409, Code: "version_conflict"}
				}
			}
		}
		if index < 0 && mutation.ExpectedRevision != 0 {
			return out, &api.APIError{Status: 409, Code: "version_conflict"}
		}
		if index < 0 {
			next.Records = append(next.Records, state)
		} else {
			next.Records[index] = state
		}
		out.Records = append(out.Records, state)
	}
	if c.records == nil {
		c.records = map[string]api.VaultRecordsResult{}
	}
	if c.recordOperations == nil {
		c.recordOperations = map[string]api.VaultRecordsResult{}
		c.recordOperationRequests = map[string]api.VaultRecordsPut{}
	}
	c.records[key] = next
	c.recordOperations[in.OperationID] = out
	c.recordOperationRequests[in.OperationID] = in
	c.events = append(c.events, "records")
	if c.recordFailure == "lost" {
		c.recordFailure = ""
		return api.VaultRecordsResult{}, errors.New("lost record acknowledgement")
	}
	return out, nil
}

func TestSingleRecordMutationSkipsSnapshotAndResumesExactCiphertext(t *testing.T) {
	ctx := context.Background()
	v, c, store := newVaultScopesFixture(t)
	c.recordFailure = "lost"
	if err := v.SetScopeVariable(ctx, "personal", v.AccountID, "", "VALUE", []byte("first")); err == nil {
		t.Fatal("lost ACK hidden")
	}
	local, err := store.LoadPasswordVault(v.Issuer, v.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if local.Operation == nil || local.Operation.Kind != "records-put" {
		local.Clear()
		t.Fatal("lost ACK journal absent")
	}
	request := bytes.Clone(local.Operation.Request)
	local.Clear()
	if err := v.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if c.recordSnapshots != 0 || c.recordGets != 1 || len(c.recordPuts) != 2 || !reflect.DeepEqual(c.recordPuts[0], c.recordPuts[1]) {
		t.Fatal("single setter fetched snapshot or changed retry")
	}
	encoded, _ := json.Marshal(c.recordPuts[1])
	if !bytes.Equal(encoded, request) {
		t.Fatal("saved ciphertext not replayed exactly")
	}
	anchor := c.scopes[scopeControlKey("personal", v.AccountID, "")]
	priorPuts := len(c.scopePuts)
	c.recordFailure = "conflict"
	err = v.SetScopeVariable(ctx, "personal", v.AccountID, "", "VALUE", []byte("competing"))
	var conflict *ScopeRefreshConflict
	if !errors.As(err, &conflict) {
		t.Fatal("record conflict not actionable", err)
	}
	local, err = store.LoadPasswordVault(v.Issuer, v.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Clear()
	if local.Operation != nil || len(c.scopePuts) != priorPuts || c.scopes[scopeControlKey("personal", v.AccountID, "")].DocumentID != anchor.DocumentID || c.recordSnapshots != 0 {
		t.Fatal("record conflict altered anchor or retained rejected operation")
	}
	keys, err := env.ParseVaultKeys(local.Payload)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	scope, err := anchor.Decode()
	if err != nil {
		t.Fatal(err)
	}
	key, err := v.recordKey(&keys, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	state := c.records[recordControlKey(recordCoordinate(scope))].Records[0]
	parsed, err := state.Decode()
	if err != nil {
		t.Fatal(err)
	}
	_, value, err := env.OpenVaultRecord(ctx, parsed, key)
	if err != nil || string(value) != "first" {
		clear(value)
		t.Fatal("rejected CAS overwrote committed value", err)
	}
	clear(value)
}
