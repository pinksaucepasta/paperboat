package main

import (
	"context"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type layerRecipient struct {
	RecipientAccount       string `json:"recipient_account"`
	MachineID              string `json:"machine_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
	HostKeyGeneration      uint64 `json:"host_key_generation"`
	HostPublic             string `json:"host_public"`
	DeliveryGeneration     uint64 `json:"delivery_generation"`
	DocumentID             string `json:"document_id"`
	FenceGeneration        uint64 `json:"fence_generation"`
}

// hostLayer encrypts exactly one authorized source for one enrolled recipient.
// It does not combine workspaces, select variable names, or grant host access.
func (e *engine) hostLayer(ctx context.Context, r request) (any, error) {
	if err := e.current(r.Vault); err != nil {
		return nil, err
	}
	if r.Scope == nil || r.LayerRecipient == nil || r.Operation == "" || r.WorkspaceID != r.Scope.WorkspaceID {
		return nil, env.ErrInvalid
	}
	scope, err := scopeDocument(*r.Scope)
	if err != nil || scope.Claims.Issuer != e.head.Issuer {
		return nil, env.ErrInvalid
	}
	c := scope.Claims
	var key []byte
	var epoch uint64
	if c.OwnerKind == "personal" {
		if c.OwnerID != e.head.AccountID {
			return nil, env.ErrInvalid
		}
		key, epoch = e.keys.PersonalKey, e.keys.PersonalEpoch
	} else {
		if r.Team == nil || r.Team.ID != c.OwnerID {
			return nil, env.ErrInvalid
		}
		key, _, err = e.teamKey(*r.Team, "read")
		if err != nil {
			return nil, err
		}
		epoch = r.Team.Epoch
	}
	if epoch != c.KeyEpoch {
		return nil, env.ErrInvalid
	}
	// The stable anchor contains no values. Device delivery grants only its
	// derived scope key; new encrypted variable records reuse this grant.
	values, err := env.OpenVaultScope(ctx, scope, key)
	if err != nil {
		return nil, err
	}
	for _, value := range values {
		clear(value)
	}
	if len(values) != 0 {
		return nil, env.ErrInvalid
	}
	scopeKey, err := env.VaultRecordKey(key, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, c.KeyEpoch)
	if err != nil {
		return nil, err
	}
	defer clear(scopeKey)
	recipient := r.LayerRecipient
	public, err := decode(recipient.HostPublic)
	if err != nil {
		return nil, env.ErrInvalid
	}
	var previous env.DocumentID
	if recipient.DeliveryGeneration != 0 {
		previous, err = env.ParseDocumentID(recipient.DocumentID)
		if err != nil {
			return nil, env.ErrInvalid
		}
	} else if recipient.DocumentID != "" {
		return nil, env.ErrInvalid
	}
	layer, err := env.SealVaultScopeKey(ctx, env.VaultLayerClaims{
		Issuer: e.head.Issuer, RecipientAccount: recipient.RecipientAccount, MachineID: recipient.MachineID,
		InstallationGeneration: recipient.InstallationGeneration, HostKeyGeneration: recipient.HostKeyGeneration,
		HostPublic: public, DeliveryGeneration: recipient.DeliveryGeneration + 1, Previous: previous[:], FenceGeneration: recipient.FenceGeneration,
		Source:        env.VaultLayerSource{WorkspaceID: c.WorkspaceID, OwnerKind: c.OwnerKind, OwnerID: c.OwnerID, MachineID: c.MachineID, KeyEpoch: c.KeyEpoch, Revision: c.Revision, Digest: scope.ID[:]},
		WriterAccount: e.head.AccountID, WriterVaultGeneration: e.head.Generation,
	}, e.keys.WriterSeed, scopeKey)
	if err != nil {
		return nil, err
	}
	return map[string]string{"operation_id": r.Operation, "envelope": encoded(layer.Raw)}, nil
}
