package environmentmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
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
	return v.provisionHost(ctx, machine, selection, false)
}

func (v PasswordVault) provisionHost(ctx context.Context, machine string, selection []api.VaultHostSelection, storedSelection bool) error {
	return v.withVaultKeys(ctx, func(local *config.PasswordVaultRecord, keys *environmente2ee.VaultKeys, c VaultDataClient) error {
		hostClient, ok := v.Client.(VaultHostClient)
		if !ok {
			return environmente2ee.ErrInvalid
		}
		host, err := hostClient.GetVaultHost(ctx, machine)
		if err != nil {
			return err
		}
		if storedSelection {
			selection = append([]api.VaultHostSelection{}, host.Selection...)
			if host.Bundle.ProjectionRevision == 0 {
				return ErrIntegrity
			}
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
			if environmente2ee.ValidateVariableName(selected.Name) != nil {
				return environmente2ee.ErrInvalid
			}
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
				// A deleted value remains an authorized selection slot. Its
				// omission from the next projection removes it on the recipient.
				authorized := false
				for _, existing := range host.Selection {
					if existing == selected {
						authorized = true
						break
					}
				}
				if !authorized {
					return environmente2ee.ErrInvalid
				}
				continue
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

// HostRefreshConflict proves a staged host publication was definitively rejected
// before commit and its journal cleared. Ambiguous failures never return it.
type HostRefreshConflict struct{ Cause error }

func (e *HostRefreshConflict) Error() string { return "ENV recipient source or selection changed" }
func (e *HostRefreshConflict) Unwrap() error { return e.Cause }

type VaultHostInventoryClient interface {
	PendingVaultHosts(context.Context) ([]api.VaultHostSummary, error)
}

// ReconcileHosts refreshes only previously provisioned, caller-owned recipients.
// Collect all pages before writes: publishing shrinks the pending inventory.
func (v PasswordVault) ReconcileHosts(ctx context.Context) (int, error) {
	c, ok := v.Client.(VaultHostInventoryClient)
	if !ok {
		return 0, environmente2ee.ErrInvalid
	}
	hosts, err := c.PendingVaultHosts(ctx)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, host := range hosts {
		if err := ctx.Err(); err != nil {
			return completed, err
		}
		err := v.provisionHost(ctx, host.MachineID, nil, true)
		var conflict *HostRefreshConflict
		if errors.As(err, &conflict) {
			err = v.provisionHost(ctx, host.MachineID, nil, true)
		}
		if err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}
