package main

import (
	"context"
	"crypto/rand"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

func (e *engine) rotatePersonal(ctx context.Context, r request) (any, error) {
	if r.Action == "personal-rotate" {
		if err := e.current(r.Vault); err != nil {
			return nil, err
		}
		if len(r.Inventory) > 513 || r.KeyEpoch != e.keys.PersonalEpoch {
			return nil, env.ErrInvalid
		}
		seen := map[string]bool{}
		for _, state := range r.Inventory {
			if state.Kind != "personal" || state.Owner != e.head.AccountID || state.Epoch != e.keys.PersonalEpoch || state.Revision == 0 || seen[state.WorkspaceID+"\x00"+state.Machine] {
				return nil, env.ErrInvalid
			}
			if _, err := env.ParseDocumentID(state.DocumentID); err != nil {
				return nil, err
			}
			seen[state.WorkspaceID+"\x00"+state.Machine] = true
		}
		keys, err := cloneKeys(e.keys)
		if err != nil {
			return nil, err
		}
		clear(keys.PersonalKey)
		keys.PersonalKey = make([]byte, 32)
		if _, err = rand.Read(keys.PersonalKey); err != nil {
			keys.Clear()
			return nil, err
		}
		keys.PersonalEpoch++
		envelope, err := e.successor(ctx, keys)
		if err != nil {
			keys.Clear()
			return nil, err
		}
		return map[string]any{"envelope": envelope, "document_id": digest(e.pendingHead.ID)}, nil
	}
	h, _, err := r.Vault.verified()
	if err != nil || h != e.head || e.pendingKeys == nil || e.pendingRaw == nil || r.Scope == nil {
		return nil, env.ErrInvalid
	}
	state := *r.Scope
	scope, err := scopeDocument(state)
	if err != nil || state.Kind != "personal" || state.Owner != h.AccountID || state.Epoch != e.keys.PersonalEpoch || scope.Claims.Issuer != h.Issuer {
		return nil, env.ErrInvalid
	}
	values, err := env.OpenVaultScope(ctx, scope, e.keys.PersonalKey)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, v := range values {
			clear(v)
		}
	}()
	next, err := env.SealVaultScope(ctx, env.VaultScopeClaims{Issuer: h.Issuer, OwnerKind: "personal", OwnerID: h.AccountID, MachineID: state.Machine, WorkspaceID: state.WorkspaceID, KeyEpoch: e.pendingKeys.PersonalEpoch, Revision: scope.Claims.Revision + 1, Previous: scope.ID[:], WriterAccount: h.AccountID, WriterVaultGeneration: e.pendingHead.Generation}, e.pendingKeys.PersonalKey, e.pendingKeys.WriterSeed, values)
	if err != nil {
		return nil, err
	}
	return map[string]any{"envelope": encoded(next.Raw), "document_id": digest(next.ID)}, nil
}
