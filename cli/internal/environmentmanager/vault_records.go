package environmentmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

func (v PasswordVault) mutateScopeRecords(ctx context.Context, kind, owner, machine string, mutate func(map[string][]byte) error) error {
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *env.VaultKeys, c VaultDataClient) error {
		anchor, err := v.ensureRecordAnchor(ctx, local, keys, c, kind, owner, machine)
		if err != nil {
			return err
		}
		values, states, err := v.readScopeRecords(ctx, keys, anchor)
		if err != nil {
			return err
		}
		defer clearVaultValues(values)
		before := map[string][]byte{}
		for name, value := range values {
			before[name] = bytes.Clone(value)
		}
		defer clearVaultValues(before)
		if err := mutate(values); err != nil {
			return err
		}
		if len(values) > env.MaximumVariables {
			return env.ErrInvalid
		}
		key, err := v.recordKey(keys, anchor)
		if err != nil {
			return err
		}
		defer clear(key)
		byID := map[string]api.VaultRecordState{}
		for _, state := range states {
			byID[state.RecordID] = state
		}
		names := map[string]bool{}
		for name := range before {
			names[name] = true
		}
		for name := range values {
			names[name] = true
		}
		changes := []api.VaultRecordMutation{}
		seen := map[string]bool{}
		for name := range names {
			value, present := values[name]
			old, existed := before[name]
			if present == existed && bytes.Equal(old, value) {
				continue
			}
			id, err := env.VaultRecordIdentifier(key, name)
			if err != nil {
				return err
			}
			if seen[id] {
				return env.ErrInvalid
			}
			seen[id] = true
			previous := byID[id].Revision
			record, err := env.SealVaultRecord(ctx, env.VaultRecordClaims{Issuer: v.Issuer, WorkspaceID: anchor.Claims.WorkspaceID, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: anchor.Claims.KeyEpoch, Revision: previous + 1, Deleted: !present, WriterAccount: v.AccountID, WriterVaultGeneration: local.Head.Generation}, key, keys.WriterSeed, name, value)
			if err != nil {
				return err
			}
			changes = append(changes, api.VaultRecordMutation{ExpectedRevision: previous, Envelope: vaultEncoded(record.Raw)})
		}
		if len(changes) == 0 {
			return nil
		}
		if len(changes) > env.MaximumVariables {
			return env.ErrInvalid
		}
		op, err := newVaultOperationID()
		if err != nil {
			return err
		}
		return v.stageOperation(ctx, local, "records-put", kind, owner, machine, api.VaultRecordsPut{OperationID: op, Records: changes})
	})
}

type VaultRecordClient interface {
	GetVaultRecord(context.Context, api.VaultLayerCoordinate, string) (api.VaultRecordState, error)
	PutVaultRecords(context.Context, api.VaultLayerCoordinate, api.VaultRecordsPut) (api.VaultRecordsResult, error)
	VaultRecords(context.Context, api.VaultLayerCoordinate, uint64) (api.VaultRecordsResult, error)
}

func recordCoordinate(scope env.VaultScope) api.VaultLayerCoordinate {
	c := scope.Claims
	return api.VaultLayerCoordinate{WorkspaceID: c.WorkspaceID, OwnerKind: c.OwnerKind, OwnerID: c.OwnerID, MachineID: c.MachineID}
}
func (v PasswordVault) recordKey(keys *env.VaultKeys, scope env.VaultScope) ([]byte, error) {
	c := scope.Claims
	root, epoch, err := scopeKey(keys, c.OwnerKind, c.OwnerID, v.AccountID)
	if err != nil {
		return nil, err
	}
	if c.Issuer != v.Issuer || c.KeyEpoch != epoch {
		return nil, ErrIntegrity
	}
	return env.VaultRecordKey(root, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, c.KeyEpoch)
}

func (v PasswordVault) readScopeRecords(ctx context.Context, keys *env.VaultKeys, scope env.VaultScope) (map[string][]byte, []api.VaultRecordState, error) {
	c, ok := v.Client.(VaultRecordClient)
	if !ok {
		return nil, nil, env.ErrInvalid
	}
	key, err := v.recordKey(keys, scope)
	if err != nil {
		return nil, nil, err
	}
	defer clear(key)
	result, err := c.VaultRecords(ctx, recordCoordinate(scope), 0)
	if err != nil {
		return nil, nil, err
	}
	values := map[string][]byte{}
	success := false
	defer func() {
		if !success {
			clearVaultValues(values)
		}
	}()
	for _, state := range result.Records {
		if state.KeyEpoch != scope.Claims.KeyEpoch || state.WorkspaceID != scope.Claims.WorkspaceID || state.OwnerKind != scope.Claims.OwnerKind || state.OwnerID != scope.Claims.OwnerID || state.MachineID != scope.Claims.MachineID {
			return nil, nil, ErrIntegrity
		}
		record, err := state.Decode()
		if err != nil || record.Claims.Issuer != v.Issuer {
			return nil, nil, ErrIntegrity
		}
		name, value, err := env.OpenVaultRecord(ctx, record, key)
		if err != nil {
			return nil, nil, err
		}
		if record.Claims.Deleted {
			clear(value)
			continue
		}
		if _, exists := values[name]; exists {
			clear(value)
			return nil, nil, ErrIntegrity
		}
		values[name] = value
	}
	if len(values) > env.MaximumVariables {
		return nil, nil, ErrIntegrity
	}
	success = true
	return values, result.Records, nil
}

func (v PasswordVault) ensureRecordAnchor(ctx context.Context, local *config.PasswordVaultRecord, keys *env.VaultKeys, c VaultDataClient, kind, owner, machine string) (env.VaultScope, error) {
	state, err := readScopedVaultSource(ctx, c, v.scopeWorkspace(kind, owner), kind, owner, machine)
	if err == nil {
		scope, err := state.Decode()
		if err != nil {
			return env.VaultScope{}, ErrIntegrity
		}
		key, err := v.recordKey(keys, scope)
		clear(key)
		if err != nil || scope.Claims.WorkspaceID != v.scopeWorkspace(kind, owner) || scope.Claims.OwnerKind != kind || scope.Claims.OwnerID != owner || scope.Claims.MachineID != machine {
			return env.VaultScope{}, ErrIntegrity
		}
		root, _, err := scopeKey(keys, kind, owner, v.AccountID)
		if err != nil {
			return env.VaultScope{}, err
		}
		values, err := env.OpenVaultScope(ctx, scope, root)
		defer clearVaultValues(values)
		if err != nil || len(values) != 0 {
			return env.VaultScope{}, ErrIntegrity
		}
		return scope, nil
	}
	if !vaultAPIResourceAbsentOnly(err) {
		return env.VaultScope{}, err
	}
	root, epoch, err := scopeKey(keys, kind, owner, v.AccountID)
	if err != nil {
		return env.VaultScope{}, err
	}
	scope, err := env.SealVaultScope(ctx, env.VaultScopeClaims{Issuer: v.Issuer, WorkspaceID: v.scopeWorkspace(kind, owner), OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: epoch, Revision: 1, Previous: make([]byte, 32), WriterAccount: v.AccountID, WriterVaultGeneration: local.Head.Generation}, root, keys.WriterSeed, map[string][]byte{})
	if err != nil {
		return env.VaultScope{}, err
	}
	op, err := newVaultOperationID()
	if err != nil {
		return env.VaultScope{}, err
	}
	if err := v.stageOperation(ctx, local, "scope-put", kind, owner, machine, api.VaultScopePut{OperationID: op, Envelope: vaultEncoded(scope.Raw)}); err != nil {
		return env.VaultScope{}, err
	}
	return scope, nil
}

func (v PasswordVault) SetScopeVariable(ctx context.Context, kind, owner, machine, name string, value []byte) error {
	return v.mutateScopeRecord(ctx, kind, owner, machine, name, value, false)
}
func (v PasswordVault) RemoveScopeVariable(ctx context.Context, kind, owner, machine, name string) error {
	return v.mutateScopeRecord(ctx, kind, owner, machine, name, nil, true)
}

func (v PasswordVault) mutateScopeRecord(ctx context.Context, kind, owner, machine, name string, value []byte, deleted bool) error {
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *env.VaultKeys, c VaultDataClient) error {
		records, ok := v.Client.(VaultRecordClient)
		if !ok {
			return env.ErrInvalid
		}
		anchor, err := v.ensureRecordAnchor(ctx, local, keys, c, kind, owner, machine)
		if err != nil {
			return err
		}
		key, err := v.recordKey(keys, anchor)
		if err != nil {
			return err
		}
		defer clear(key)
		id, err := env.VaultRecordIdentifier(key, name)
		if err != nil {
			return err
		}
		coordinate := recordCoordinate(anchor)
		previous, err := records.GetVaultRecord(ctx, coordinate, id)
		if !vaultAPIResourceAbsentOnly(err) && err != nil {
			return err
		}
		var revision uint64
		if err == nil {
			old, err := previous.Decode()
			if err != nil || old.Claims.Issuer != v.Issuer || old.Claims.KeyEpoch != anchor.Claims.KeyEpoch || old.Claims.RecordID != id || old.Claims.WorkspaceID != coordinate.WorkspaceID || old.Claims.OwnerKind != coordinate.OwnerKind || old.Claims.OwnerID != coordinate.OwnerID || old.Claims.MachineID != coordinate.MachineID {
				return ErrIntegrity
			}
			revision = old.Claims.Revision
			if deleted && old.Claims.Deleted {
				return ErrVariableNotConfigured
			}
		} else if deleted {
			return ErrVariableNotConfigured
		}
		claims := env.VaultRecordClaims{Issuer: v.Issuer, WorkspaceID: coordinate.WorkspaceID, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: anchor.Claims.KeyEpoch, RecordID: id, Revision: revision + 1, Deleted: deleted, WriterAccount: v.AccountID, WriterVaultGeneration: local.Head.Generation}
		record, err := env.SealVaultRecord(ctx, claims, key, keys.WriterSeed, name, value)
		if err != nil {
			return err
		}
		op, err := newVaultOperationID()
		if err != nil {
			return err
		}
		return v.stageOperation(ctx, local, "records-put", kind, owner, machine, api.VaultRecordsPut{OperationID: op, Records: []api.VaultRecordMutation{{ExpectedRevision: revision, Envelope: vaultEncoded(record.Raw)}}})
	})
}

func (v PasswordVault) publishRecordsOperation(ctx context.Context, local *config.PasswordVaultRecord) error {
	c, ok := v.Client.(VaultRecordClient)
	if !ok {
		return env.ErrInvalid
	}
	op := local.Operation
	var in api.VaultRecordsPut
	if op == nil || json.Unmarshal(op.Request, &in) != nil || len(in.Records) == 0 || len(in.Records) > env.MaximumVariables {
		return ErrIntegrity
	}
	coordinate := api.VaultLayerCoordinate{WorkspaceID: op.WorkspaceID, OwnerKind: op.OwnerKind, OwnerID: op.OwnerID, MachineID: op.MachineID}
	out, err := c.PutVaultRecords(ctx, coordinate, in)
	if err != nil {
		var rejected *api.APIError
		if errors.As(err, &rejected) && rejected.Status == 409 && rejected.Code == "version_conflict" {
			local.Operation = nil
			if saveErr := v.Store.SavePasswordVault(*local); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return &ScopeRefreshConflict{Cause: err}
		}
		return &ScopePublicationPending{Cause: err}
	}
	if len(out.Records) != len(in.Records) {
		return ErrIntegrity
	}
	want := make([]string, len(in.Records))
	got := make([]string, len(out.Records))
	for i, r := range in.Records {
		want[i] = r.Envelope
	}
	for i, r := range out.Records {
		got[i] = r.Envelope
	}
	sort.Strings(want)
	sort.Strings(got)
	for i := range want {
		if want[i] != got[i] {
			return ErrIntegrity
		}
	}
	return nil
}
