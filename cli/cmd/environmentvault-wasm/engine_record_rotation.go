package main

import (
	"context"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type recordReplacement struct {
	ExpectedSequence uint64   `json:"expected_sequence"`
	DocumentIDs      []string `json:"document_ids"`
	Envelopes        []string `json:"envelopes"`
}

func (e *engine) rotateRecords(ctx context.Context, r request, scope env.VaultScope, oldRoot, newRoot []byte, epoch, generation uint64) (recordReplacement, error) {
	out := recordReplacement{ExpectedSequence: r.RecordSequence, DocumentIDs: []string{}, Envelopes: []string{}}
	if r.TotalLoss {
		return out, nil
	}
	if len(r.Records) > 4096 {
		return out, env.ErrInvalid
	}
	c := scope.Claims
	oldKey, err := env.VaultRecordKey(oldRoot, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, c.KeyEpoch)
	if err != nil {
		return out, err
	}
	defer clear(oldKey)
	newKey, err := env.VaultRecordKey(newRoot, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, epoch)
	if err != nil {
		return out, err
	}
	defer clear(newKey)
	liveBytes := 0
	seen := map[string]bool{}
	for _, state := range r.Records {
		record, err := recordDocument(state)
		if err != nil {
			return out, err
		}
		o := record.Claims
		if o.Issuer != c.Issuer || o.WorkspaceID != c.WorkspaceID || o.OwnerKind != c.OwnerKind || o.OwnerID != c.OwnerID || o.MachineID != c.MachineID || o.KeyEpoch != c.KeyEpoch || state.Sequence == 0 || state.Sequence > r.RecordSequence || seen[o.RecordID] {
			return out, env.ErrInvalid
		}
		seen[o.RecordID] = true
		name, value, err := env.OpenVaultRecord(ctx, record, oldKey)
		if err != nil {
			return out, err
		}
		if o.Deleted {
			clear(value)
			continue
		}
		if len(out.Envelopes) >= 128 {
			clear(value)
			return out, env.ErrInvalid
		}
		id, err := env.VaultRecordIdentifier(newKey, name)
		if err != nil {
			clear(value)
			return out, err
		}
		next, err := env.SealVaultRecord(ctx, env.VaultRecordClaims{Issuer: c.Issuer, WorkspaceID: c.WorkspaceID, OwnerKind: c.OwnerKind, OwnerID: c.OwnerID, MachineID: c.MachineID, KeyEpoch: epoch, RecordID: id, Revision: 1, WriterAccount: e.head.AccountID, WriterVaultGeneration: generation}, newKey, e.keys.WriterSeed, name, value)
		clear(value)
		if err != nil {
			return out, err
		}
		bytes, err := next.CiphertextBytes()
		if err != nil {
			return out, err
		}
		liveBytes += bytes
		if liveBytes > 256<<10 {
			return out, env.ErrInvalid
		}
		out.DocumentIDs = append(out.DocumentIDs, state.DocumentID)
		out.Envelopes = append(out.Envelopes, encoded(next.Raw))
	}
	return out, nil
}
