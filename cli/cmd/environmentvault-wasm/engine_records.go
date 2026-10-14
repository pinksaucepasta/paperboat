package main

import (
	"context"
	"errors"

	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type recordState struct {
	WorkspaceID  string `json:"workspace_id"`
	Kind         string `json:"owner_kind"`
	Owner        string `json:"owner_id"`
	Machine      string `json:"machine_id"`
	Epoch        uint64 `json:"key_epoch"`
	RecordID     string `json:"record_id"`
	Revision     uint64 `json:"revision"`
	Deleted      bool   `json:"deleted"`
	Sequence     uint64 `json:"sequence"`
	DocumentID   string `json:"document_id"`
	WriterPublic string `json:"writer_public"`
	Envelope     string `json:"envelope"`
}

func recordDocument(s recordState) (env.VaultRecord, error) {
	raw, err := decode(s.Envelope)
	if err != nil {
		return env.VaultRecord{}, err
	}
	writer, err := decode(s.WriterPublic)
	if err != nil {
		return env.VaultRecord{}, err
	}
	record, err := env.ParseVaultRecord(raw, writer)
	if err != nil {
		return env.VaultRecord{}, err
	}
	c := record.Claims
	if digest(record.ID) != s.DocumentID || c.WorkspaceID != s.WorkspaceID || c.OwnerKind != s.Kind || c.OwnerID != s.Owner || c.MachineID != s.Machine || c.KeyEpoch != s.Epoch || c.RecordID != s.RecordID || c.Revision != s.Revision || c.Deleted != s.Deleted {
		return env.VaultRecord{}, env.ErrInvalid
	}
	return record, nil
}

func (e *engine) recordRoot(kind, owner string) ([]byte, uint64, error) {
	if kind == "personal" && owner == e.head.AccountID {
		return e.keys.PersonalKey, e.keys.PersonalEpoch, nil
	}
	if kind == "team" {
		for _, key := range e.keys.Teams {
			if key.TeamID == owner {
				return key.Key, key.Epoch, nil
			}
		}
		return nil, 0, errors.New("The current Team ENV key is not in this vault. Refresh Team key access before retrying.")
	}
	return nil, 0, env.ErrInvalid
}

func (e *engine) recordOperation(ctx context.Context, r request) (any, error) {
	if err := e.current(r.Vault); err != nil {
		return nil, err
	}
	if r.Action == "record-id" || r.Action == "record-write" {
		if err := env.ValidateVariableName(r.Name); err != nil {
			return nil, err
		}
	}
	workspace, kind, owner, machine := r.WorkspaceID, r.Kind, r.Owner, r.Machine
	if r.Action == "record-read" {
		if r.Record == nil {
			return nil, env.ErrInvalid
		}
		workspace, kind, owner, machine = r.Record.WorkspaceID, r.Record.Kind, r.Record.Owner, r.Record.Machine
	}
	root, epoch, err := e.recordRoot(kind, owner)
	if err != nil {
		return nil, err
	}
	key, err := env.VaultRecordKey(root, workspace, kind, owner, machine, epoch)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	switch r.Action {
	case "record-id":
		id, err := env.VaultRecordIdentifier(key, r.Name)
		if err != nil {
			return nil, err
		}
		return map[string]string{"record_id": id}, nil
	case "record-read":
		if r.Record.Epoch != epoch {
			return nil, env.ErrInvalid
		}
		record, err := recordDocument(*r.Record)
		if err != nil {
			return nil, err
		}
		if record.Claims.Issuer != e.head.Issuer {
			return nil, env.ErrInvalid
		}
		name, value, err := env.OpenVaultRecord(ctx, record, key)
		if err != nil {
			return nil, err
		}
		defer clear(value)
		return map[string]any{"name": name, "value": string(value), "deleted": record.Claims.Deleted}, nil
	case "scope-create":
		if r.Scope != nil {
			return nil, env.ErrInvalid
		}
		anchor, err := env.SealVaultScope(ctx, env.VaultScopeClaims{Issuer: e.head.Issuer, WorkspaceID: workspace, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: epoch, Revision: 1, Previous: make([]byte, 32), WriterAccount: e.head.AccountID, WriterVaultGeneration: e.head.Generation}, root, e.keys.WriterSeed, map[string][]byte{})
		if err != nil {
			return nil, err
		}
		return map[string]string{"envelope": encoded(anchor.Raw)}, nil
	case "record-write":
		if r.Scope == nil {
			return nil, env.ErrInvalid
		}
		anchor, err := scopeDocument(*r.Scope)
		if err != nil {
			return nil, err
		}
		c := anchor.Claims
		if c.Issuer != e.head.Issuer || c.WorkspaceID != workspace || c.OwnerKind != kind || c.OwnerID != owner || c.MachineID != machine || c.KeyEpoch != epoch {
			return nil, env.ErrInvalid
		}
		id, err := env.VaultRecordIdentifier(key, r.Name)
		if err != nil {
			return nil, err
		}
		var previous uint64
		if r.Record != nil {
			old, err := recordDocument(*r.Record)
			if err != nil {
				return nil, err
			}
			o := old.Claims
			if o.Issuer != e.head.Issuer || o.WorkspaceID != workspace || o.OwnerKind != kind || o.OwnerID != owner || o.MachineID != machine || o.KeyEpoch != epoch || o.RecordID != id {
				return nil, env.ErrInvalid
			}
			previous = o.Revision
		}
		value := []byte(r.Value)
		defer clear(value)
		record, err := env.SealVaultRecord(ctx, env.VaultRecordClaims{Issuer: e.head.Issuer, WorkspaceID: workspace, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: epoch, RecordID: id, Revision: previous + 1, Deleted: r.Deleted, WriterAccount: e.head.AccountID, WriterVaultGeneration: e.head.Generation}, key, e.keys.WriterSeed, r.Name, value)
		if err != nil {
			return nil, err
		}
		return map[string]any{"expected_revision": previous, "envelope": encoded(record.Raw)}, nil
	}
	return nil, env.ErrInvalid
}
