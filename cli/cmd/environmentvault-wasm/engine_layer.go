package main

import (
	"context"
	"errors"
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
	if err := authorizeLayerRecipient(e.head.AccountID, c, r.Team, *r.LayerRecipient); err != nil {
		return nil, err
	}
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

// authorizeLayerRecipient refuses recipients the control plane may list but this
// writer must not seal to: Personal scope keys go only to the writer's own
// account and machine, and Team scope keys only to active members who may read.
// The host public key itself is validated by SealVaultScopeKey (X25519, not
// low-order); no key is trusted for any other account.
func authorizeLayerRecipient(account string, c env.VaultScopeClaims, team *teamState, recipient layerRecipient) error {
	if recipient.MachineID == "" || recipient.RecipientAccount == "" || recipient.HostKeyGeneration == 0 || recipient.InstallationGeneration == 0 {
		return env.ErrInvalid
	}
	if c.MachineID != "" && c.MachineID != recipient.MachineID {
		return errors.New("The device override belongs to a different machine than this recipient.")
	}
	if c.OwnerKind == "personal" {
		if recipient.RecipientAccount != account || c.OwnerID != account {
			return errors.New("Private ENV values can only be sent to your own devices.")
		}
		return nil
	}
	if team == nil {
		return env.ErrInvalid
	}
	if _, ok := team.actor(recipient.RecipientAccount, "read"); !ok {
		return errors.New("The recipient is not an active Team member allowed to read ENV values.")
	}
	return nil
}
