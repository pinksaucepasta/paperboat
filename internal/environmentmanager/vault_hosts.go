package environmentmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type VaultHostClient interface {
	GetVaultHost(context.Context, string) (api.VaultHostState, error)
	ProvisionVaultHost(context.Context, string, api.VaultHostProvision) (environmente2ee.ProjectionBundle, error)
}

// ProvisionHost encrypts only the explicit selected values plus this machine's
// overrides. Reprovisioning never transmits a personal/team encryption key.
func (v PasswordVault) ProvisionHost(ctx context.Context, machine string, selection []api.VaultHostSelection) error {
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
		hostClient, ok := v.Client.(VaultHostClient)
		if !ok {
			return environmente2ee.ErrInvalid
		}
		host, err := hostClient.GetVaultHost(ctx, machine)
		if err != nil {
			return err
		}
		b := host.Bundle
		if b.AccountID != v.AccountID || b.MachineID != machine || b.State == "revoked" || len(selection) > environmente2ee.MaximumVariables {
			return ErrIntegrity
		}
		public, err := base64.RawURLEncoding.Strict().DecodeString(b.HostPublic)
		if err != nil {
			return ErrIntegrity
		}
		var previous environmente2ee.DocumentID
		if b.ProjectionRevision > 0 {
			previous, err = environmente2ee.ParseDocumentID(b.DocumentID)
			if err != nil {
				return ErrIntegrity
			}
		}
		base := map[string][]byte{}
		defer clearVaultValues(base)
		scopes := map[string]map[string][]byte{}
		defer func() {
			for _, values := range scopes {
				clearVaultValues(values)
			}
		}()
		sources := []environmente2ee.ProjectionSource{}
		seenNames := map[string]bool{}
		for _, selected := range selection {
			if seenNames[strings.ToUpper(selected.Name)] {
				return environmente2ee.ErrInvalid
			}
			seenNames[strings.ToUpper(selected.Name)] = true
			coordinate := selected.OwnerKind + "\x00" + selected.OwnerID
			values, ok := scopes[coordinate]
			if !ok {
				scope, decoded, err := v.readVaultScope(ctx, c, keys, selected.OwnerKind, selected.OwnerID, "")
				if err != nil {
					return err
				}
				values = decoded
				scopes[coordinate] = values
				sources = append(sources, environmente2ee.ProjectionSource{OwnerKind: selected.OwnerKind, OwnerID: selected.OwnerID, KeyEpoch: scope.Claims.KeyEpoch, Revision: scope.Claims.Revision, Digest: scope.ID[:]})
			}
			value, ok := values[selected.Name]
			if !ok {
				return environmente2ee.ErrInvalid
			}
			base[selected.Name] = bytes.Clone(value)
		}
		machineScope, overrides, err := v.readVaultScope(ctx, c, keys, "personal", v.AccountID, machine)
		if api.IsNotFound(err) {
			overrides = map[string][]byte{}
		} else if err != nil {
			return err
		} else {
			sources = append(sources, environmente2ee.ProjectionSource{OwnerKind: "personal", OwnerID: v.AccountID, MachineID: machine, KeyEpoch: machineScope.Claims.KeyEpoch, Revision: machineScope.Claims.Revision, Digest: machineScope.ID[:]})
		}
		defer clearVaultValues(overrides)
		effective, err := environmente2ee.MergeScopes(base, overrides)
		if err != nil {
			return err
		}
		defer clearVaultValues(effective)
		projection, err := environmente2ee.SealHostProjection(ctx, environmente2ee.HostProjectionClaims{Issuer: v.Issuer, OwnerAccount: v.AccountID, MachineID: machine, InstallationGeneration: b.InstallationGeneration, HostKeyGeneration: b.HostKeyGeneration, HostPublic: public, SelectionGeneration: b.SelectionGeneration + 1, Revision: b.ProjectionRevision + 1, Previous: previous[:], Sources: sources, WriterVaultGeneration: local.Head.Generation}, keys.WriterSeed, effective)
		if err != nil {
			return err
		}
		op, err := newVaultOperationID()
		if err != nil {
			return err
		}
		return v.stageOperation(ctx, local, "host-provision", "personal", v.AccountID, machine, api.VaultHostProvision{OperationID: op, ExpectedSelectionGeneration: b.SelectionGeneration, Selection: append([]api.VaultHostSelection{}, selection...), Envelope: vaultEncoded(projection.Raw)})
	})
}
