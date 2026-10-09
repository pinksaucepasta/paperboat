package environmentmanager

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type vaultWorkspaceSourceClient interface {
	GetVaultScopeInWorkspace(context.Context, string, string, string, string) (api.VaultScopeState, error)
	PutVaultScopeInWorkspace(context.Context, string, string, string, string, api.VaultScopePut) (api.VaultScopeState, error)
}

func readScopedVaultSource(ctx context.Context, c VaultDataClient, workspace, kind, owner, machine string) (api.VaultScopeState, error) {
	if scoped, ok := c.(vaultWorkspaceSourceClient); ok {
		return scoped.GetVaultScopeInWorkspace(ctx, workspace, kind, owner, machine)
	}
	return c.GetVaultScope(ctx, kind, owner, machine)
}
func putScopedVaultSource(ctx context.Context, c VaultDataClient, workspace, kind, owner, machine string, in api.VaultScopePut) (api.VaultScopeState, error) {
	if scoped, ok := c.(vaultWorkspaceSourceClient); ok {
		return scoped.PutVaultScopeInWorkspace(ctx, workspace, kind, owner, machine, in)
	}
	return c.PutVaultScope(ctx, kind, owner, machine, in)
}

type VaultLayerClient interface {
	VaultLayerRecipients(context.Context, api.VaultLayerCoordinate) ([]api.VaultLayerRecipient, error)
	PutVaultLayer(context.Context, string, string, api.VaultLayerPut) (api.VaultLayerDelivery, error)
}

// RefreshSource delivers a complete encrypted source to its currently authorized
// recipients. Recipient discovery grants no new device or Team access.
func (v PasswordVault) RefreshSource(ctx context.Context, kind, owner, machine string) (int, error) {
	count := 0
	err := v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *env.VaultKeys, c VaultDataClient) error {
		layers, ok := v.Client.(VaultLayerClient)
		if !ok {
			return env.ErrInvalid
		}
		scope, err := v.ensureRecordAnchor(ctx, local, keys, c, kind, owner, machine)
		if err != nil {
			return err
		}
		key, err := v.recordKey(keys, scope)
		if err != nil {
			return err
		}
		defer clear(key)
		coordinate := api.VaultLayerCoordinate{WorkspaceID: scope.Claims.WorkspaceID, OwnerKind: kind, OwnerID: owner, MachineID: machine}
		recipients, err := layers.VaultLayerRecipients(ctx, coordinate)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, recipient := range recipients {
			if err := ctx.Err(); err != nil {
				return err
			}
			if seen[recipient.MachineID] {
				return ErrIntegrity
			}
			seen[recipient.MachineID] = true
			public, err := base64.RawURLEncoding.Strict().DecodeString(recipient.HostPublic)
			if err != nil {
				return ErrIntegrity
			}
			var previous env.DocumentID
			if recipient.DeliveryGeneration > 0 {
				previous, err = env.ParseDocumentID(recipient.DocumentID)
				if err != nil {
					return ErrIntegrity
				}
			} else if recipient.DocumentID != "" {
				return ErrIntegrity
			}
			layer, err := env.SealVaultScopeKey(ctx, env.VaultLayerClaims{Issuer: v.Issuer, RecipientAccount: recipient.RecipientAccount, MachineID: recipient.MachineID, InstallationGeneration: recipient.InstallationGeneration, HostKeyGeneration: recipient.HostKeyGeneration, HostPublic: public, DeliveryGeneration: recipient.DeliveryGeneration + 1, Previous: previous[:], FenceGeneration: recipient.FenceGeneration, Source: env.VaultLayerSource{WorkspaceID: scope.Claims.WorkspaceID, OwnerKind: kind, OwnerID: owner, MachineID: machine, KeyEpoch: scope.Claims.KeyEpoch, Revision: scope.Claims.Revision, Digest: scope.ID[:]}, WriterAccount: v.AccountID, WriterVaultGeneration: local.Head.Generation}, keys.WriterSeed, key)
			if err != nil {
				return err
			}
			operation, err := newVaultOperationID()
			if err != nil {
				return err
			}
			if err = v.stageOperation(ctx, local, "layer-put", kind, owner, recipient.MachineID, api.VaultLayerPut{OperationID: operation, Envelope: vaultEncoded(layer.Raw)}); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}
func (v PasswordVault) publishLayerOperation(ctx context.Context, local *config.PasswordVaultRecord) error {
	client, ok := v.Client.(VaultLayerClient)
	if !ok {
		return env.ErrInvalid
	}
	op := local.Operation
	var in api.VaultLayerPut
	if op == nil || json.Unmarshal(op.Request, &in) != nil {
		return ErrIntegrity
	}
	out, err := client.PutVaultLayer(ctx, op.WorkspaceID, op.MachineID, in)
	if err != nil {
		var rejected *api.APIError
		if errors.As(err, &rejected) && (rejected.Status == 403 && rejected.Code == "permission_denied" || rejected.Status == 409 && rejected.Code == "rotation_required") {
			if local.Pending != nil {
				return &LayerPublicationPending{Cause: err}
			}
			local.Operation = nil
			if saveErr := v.Store.SavePasswordVault(*local); saveErr != nil {
				return &LayerPublicationPending{Cause: errors.Join(err, saveErr)}
			}
			return &LayerRefreshConflict{Cause: err, AuthorityRevoked: rejected.Status == 403, RotationRequired: rejected.Code == "rotation_required"}
		}
		if errors.As(err, &rejected) && rejected.Status == 409 && rejected.Code == "version_conflict" {
			return v.reconcileLayerConflict(ctx, local, in, err)
		}
		return &LayerPublicationPending{Cause: err}
	}
	if out.Envelope != in.Envelope {
		return ErrIntegrity
	}
	return nil
}

type LayerPublicationPending struct{ Cause error }

func (e *LayerPublicationPending) Error() string {
	return "Encrypted ENV layer publication is pending; resume the saved operation before retrying"
}
func (e *LayerPublicationPending) Unwrap() error { return e.Cause }

// ReconcileLayers accepts current addressed Team keys, refreshes private heads
// across all workspaces, then current Team globals. Server discovery restricts
// read-only members to their own devices.
func (v PasswordVault) ReconcileLayers(ctx context.Context) (int, error) {
	if err := v.SyncTeamGrants(ctx); err != nil {
		return 0, err
	}
	inventory, ok := v.Client.(interface {
		GetVaultPersonalScopes(context.Context) (api.VaultPersonalInventory, error)
	})
	if !ok {
		return 0, env.ErrInvalid
	}
	heads, err := inventory.GetVaultPersonalScopes(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	seen := map[string]bool{}
	for _, head := range heads.Scopes {
		coordinate := head.WorkspaceID + "\x00" + head.MachineID
		if head.OwnerKind != "personal" || head.OwnerID != v.AccountID || seen[coordinate] {
			return count, ErrIntegrity
		}
		seen[coordinate] = true
		scoped := v
		scoped.WorkspaceID = head.WorkspaceID
		n, err := scoped.RefreshSource(ctx, "personal", v.AccountID, head.MachineID)
		count += n
		if err != nil {
			return count, err
		}
	}
	data, err := v.dataClient()
	if err != nil {
		return count, err
	}
	grants, err := data.GetVaultGrants(ctx)
	if err != nil {
		return count, err
	}
	teams := map[string]bool{}
	for _, grant := range grants {
		teams[grant.TeamID] = true
	}
	if v.WorkspaceID != "personal" {
		teams[v.WorkspaceID] = true
	}
	teamIDs := make([]string, 0, len(teams))
	for teamID := range teams {
		teamIDs = append(teamIDs, teamID)
	}
	sort.Strings(teamIDs)
	for _, teamID := range teamIDs {
		scoped := v
		scoped.WorkspaceID = teamID
		n, err := scoped.RefreshSource(ctx, "team", teamID, "")
		count += n
		if err != nil && !vaultAPIResourceAbsentOnly(err) {
			return count, err
		}
	}
	return count, nil
}

// A typed CAS rejection is reconciled against the authorized current source and
// recipient cursor. Ambiguous network failures retain the exact ciphertext.
func (v PasswordVault) reconcileLayerConflict(ctx context.Context, local *config.PasswordVaultRecord, in api.VaultLayerPut, rejected error) error {
	pending := func(err error) error { return &LayerPublicationPending{Cause: errors.Join(rejected, err)} }
	if local.Pending != nil || local.Operation == nil || local.Operation.Kind != "layer-put" || len(local.Payload) == 0 {
		return pending(ErrIntegrity)
	}
	keys, err := env.ParseVaultKeys(local.Payload)
	if err != nil {
		return pending(ErrIntegrity)
	}
	defer keys.Clear()
	signer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	defer clear(signer)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(in.Envelope)
	if err != nil {
		return pending(ErrIntegrity)
	}
	candidate, err := env.ParseVaultLayer(raw, signer.Public().(ed25519.PublicKey))
	if err != nil {
		return pending(ErrIntegrity)
	}
	claims := candidate.Claims
	op := local.Operation
	if claims.Issuer != v.Issuer || claims.WriterAccount != v.AccountID || claims.WriterVaultGeneration != local.Head.Generation || claims.Source.WorkspaceID != op.WorkspaceID || claims.MachineID != op.MachineID {
		return pending(ErrIntegrity)
	}
	data, err := v.dataClient()
	if err != nil {
		return pending(err)
	}
	state, err := readScopedVaultSource(ctx, data, op.WorkspaceID, claims.Source.OwnerKind, claims.Source.OwnerID, claims.Source.MachineID)
	if err != nil {
		return pending(err)
	}
	source, err := state.Decode()
	if err != nil || source.Claims.WorkspaceID != claims.Source.WorkspaceID || source.Claims.OwnerKind != claims.Source.OwnerKind || source.Claims.OwnerID != claims.Source.OwnerID || source.Claims.MachineID != claims.Source.MachineID || source.Claims.KeyEpoch < claims.Source.KeyEpoch || source.Claims.KeyEpoch == claims.Source.KeyEpoch && source.Claims.Revision < claims.Source.Revision {
		return pending(ErrIntegrity)
	}
	client, ok := v.Client.(VaultLayerClient)
	if !ok {
		return pending(ErrIntegrity)
	}
	// Pending-filtered discovery is not sufficient proof of a committed candidate.
	reader, ok := client.(interface {
		VaultAllLayerRecipients(context.Context, api.VaultLayerCoordinate) ([]api.VaultLayerRecipient, error)
	})
	if !ok {
		return pending(ErrIntegrity)
	}
	recipients, err := reader.VaultAllLayerRecipients(ctx, api.VaultLayerCoordinate{WorkspaceID: claims.Source.WorkspaceID, OwnerKind: claims.Source.OwnerKind, OwnerID: claims.Source.OwnerID, MachineID: claims.Source.MachineID})
	if err != nil {
		return pending(err)
	}
	for _, r := range recipients {
		if r.MachineID != claims.MachineID {
			continue
		}
		if r.RecipientAccount == claims.RecipientAccount && (r.InstallationGeneration > claims.InstallationGeneration || r.HostKeyGeneration > claims.HostKeyGeneration) {
			local.Operation = nil
			if err := v.Store.SavePasswordVault(*local); err != nil {
				return pending(err)
			}
			return &LayerRefreshConflict{Cause: rejected}
		}
		if r.RecipientAccount != claims.RecipientAccount || r.InstallationGeneration != claims.InstallationGeneration || r.HostKeyGeneration != claims.HostKeyGeneration || r.HostPublic != base64.RawURLEncoding.EncodeToString(claims.HostPublic) || r.FenceGeneration < claims.FenceGeneration {
			return pending(ErrIntegrity)
		}
		committed := r.DeliveryGeneration == claims.DeliveryGeneration && r.DocumentID == candidate.ID.String()
		superseded := r.DeliveryGeneration >= claims.DeliveryGeneration || r.FenceGeneration > claims.FenceGeneration || source.Claims.KeyEpoch > claims.Source.KeyEpoch || source.Claims.Revision > claims.Source.Revision
		if !committed && !superseded {
			return pending(ErrIntegrity)
		}
		local.Operation = nil
		if err := v.Store.SavePasswordVault(*local); err != nil {
			return pending(err)
		}
		if committed {
			return nil
		}
		return &LayerRefreshConflict{Cause: rejected}
	}
	// An all-state Personal inventory is owner-complete. Team inventory is complete
	// across members only for a still-authorized writer.
	complete := claims.Source.OwnerKind == "personal"
	if !complete {
		team, err := data.GetVaultTeam(ctx, claims.Source.OwnerID)
		if err != nil {
			return pending(err)
		}
		_, complete = vaultTeamActor(team, v.AccountID, "write")
	}
	if complete {
		local.Operation = nil
		if err := v.Store.SavePasswordVault(*local); err != nil {
			return pending(err)
		}
		return &LayerRefreshConflict{Cause: rejected}
	}
	return pending(ErrIntegrity)
}

type LayerRefreshConflict struct {
	RotationRequired bool
	Cause            error
	AuthorityRevoked bool
}

func (e *LayerRefreshConflict) Error() string {
	if e.AuthorityRevoked {
		return "ENV delivery authority was revoked; the saved source remains unchanged"
	}
	return "ENV delivery changed; resume to refresh current authorized devices from the saved source"
}
func (e *LayerRefreshConflict) Unwrap() error { return e.Cause }
