package environmentmanager

import (
	"context"
	"sort"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

// prepareRecordReplacement freezes the exact old live inventory and encrypts
// each value once under the new scope epoch. The caller journals this entire
// replacement before publishing, so retries never regenerate ciphertext.
func (v PasswordVault) prepareRecordReplacement(ctx context.Context, previous, next env.VaultScope, oldKeys, nextKeys *env.VaultKeys, writerGeneration uint64) (api.VaultRecordReplacement, error) {
	out := api.VaultRecordReplacement{DocumentIDs: []string{}, Envelopes: []string{}}
	c, ok := v.Client.(VaultRecordClient)
	if !ok {
		return out, env.ErrInvalid
	}
	coordinate := recordCoordinate(previous)
	if previous.Claims.Issuer != v.Issuer || next.Claims.Issuer != v.Issuer || recordCoordinate(next) != coordinate || next.Claims.KeyEpoch <= previous.Claims.KeyEpoch || writerGeneration == 0 {
		return out, ErrIntegrity
	}
	result, err := c.VaultRecords(ctx, coordinate, 0)
	if err != nil {
		return out, err
	}
	if result.Sequence > env.MaximumContractInteger || len(result.Records) > 4096 {
		return out, ErrIntegrity
	}
	out.ExpectedSequence = result.Sequence
	oldKey, err := v.recordKey(oldKeys, previous)
	if err != nil {
		return out, err
	}
	defer clear(oldKey)
	nextKey, err := v.recordKey(nextKeys, next)
	if err != nil {
		return out, err
	}
	defer clear(nextKey)
	seen := map[string]bool{}
	names := map[string]bool{}
	liveBytes := 0
	for _, state := range result.Records {
		if state.WorkspaceID != coordinate.WorkspaceID || state.OwnerKind != coordinate.OwnerKind || state.OwnerID != coordinate.OwnerID || state.MachineID != coordinate.MachineID || state.KeyEpoch != previous.Claims.KeyEpoch || state.Sequence == 0 || state.Sequence > result.Sequence || seen[state.RecordID] {
			return out, ErrIntegrity
		}
		seen[state.RecordID] = true
		record, err := state.Decode()
		if err != nil || record.Claims.Issuer != v.Issuer {
			return out, ErrIntegrity
		}
		name, value, err := env.OpenVaultRecord(ctx, record, oldKey)
		if err != nil {
			return out, err
		}
		if state.Deleted {
			clear(value)
			continue
		}
		if names[name] {
			clear(value)
			return out, ErrIntegrity
		}
		names[name] = true
		replacement, err := env.SealVaultRecord(ctx, env.VaultRecordClaims{Issuer: v.Issuer, WorkspaceID: coordinate.WorkspaceID, OwnerKind: coordinate.OwnerKind, OwnerID: coordinate.OwnerID, MachineID: coordinate.MachineID, KeyEpoch: next.Claims.KeyEpoch, Revision: 1, WriterAccount: v.AccountID, WriterVaultGeneration: writerGeneration}, nextKey, nextKeys.WriterSeed, name, value)
		clear(value)
		if err != nil {
			return out, err
		}
		ciphertextBytes, err := replacement.CiphertextBytes()
		if err != nil {
			return out, err
		}
		liveBytes += ciphertextBytes
		if len(names) > env.MaximumVariables || liveBytes > 256<<10 {
			return out, env.ErrInvalid
		}
		out.DocumentIDs = append(out.DocumentIDs, state.DocumentID)
		out.Envelopes = append(out.Envelopes, vaultEncoded(replacement.Raw))
	}
	sort.Strings(out.DocumentIDs)
	return out, nil
}
