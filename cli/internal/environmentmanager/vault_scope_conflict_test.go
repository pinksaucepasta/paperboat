package environmentmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type scopeConflictControl struct {
	*vaultScopesControl
	mode     string
	current  api.VaultScopeState
	requests []api.VaultScopePut
	readErr  error
}

func (c *scopeConflictControl) GetVaultScope(ctx context.Context, kind, owner, machine string) (api.VaultScopeState, error) {
	if c.mode == "" {
		return c.vaultScopesControl.GetVaultScope(ctx, kind, owner, machine)
	}
	return c.current, c.readErr
}
func (c *scopeConflictControl) PutVaultScope(ctx context.Context, kind, owner, machine string, in api.VaultScopePut) (api.VaultScopeState, error) {
	if c.mode == "" {
		return c.vaultScopesControl.PutVaultScope(ctx, kind, owner, machine, in)
	}
	c.requests = append(c.requests, in)
	return api.VaultScopeState{}, &api.APIError{Status: 409, Code: "version_conflict"}
}
func (c *scopeConflictControl) GetVaultSharing(context.Context, string) (api.VaultSharingState, error) {
	return api.VaultSharingState{AccountID: c.account, VaultGeneration: 1, WriterPublic: base64.RawURLEncoding.EncodeToString(c.writerPublic)}, nil
}
func TestVaultScopeConflictReconcilesOnlyVerifiedCurrentCursor(t *testing.T) {
	for _, mode := range []string{"committed", "competing", "later", "epoch", "stale", "wrong-target", "invalid-signature", "forbidden", "ambiguous", "pending-successor"} {
		t.Run(mode, func(t *testing.T) {
			v, control, store := newVaultScopesFixture(t)
			ctx := context.Background()
			before, keys := loadVaultKeysForScopeTest(t, store)
			defer before.Clear()
			defer keys.Clear()
			claims := environmente2ee.VaultScopeClaims{Issuer: v.Issuer, WorkspaceID: "personal", OwnerKind: "personal", OwnerID: v.AccountID, KeyEpoch: keys.PersonalEpoch, Revision: 1, Previous: make([]byte, 32), WriterAccount: v.AccountID, WriterVaultGeneration: before.Head.Generation}
			candidate, err := environmente2ee.SealVaultScope(ctx, claims, keys.PersonalKey, keys.WriterSeed, map[string][]byte{})
			if err != nil {
				t.Fatal(err)
			}
			op := api.VaultScopePut{OperationID: "operation_conflict_candidate", Envelope: vaultEncoded(candidate.Raw)}
			request, err := json.Marshal(op)
			if err != nil {
				t.Fatal(err)
			}
			record, err := store.LoadPasswordVault(v.Issuer, v.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			record.Operation = &config.VaultOperation{WorkspaceID: "personal", Kind: "scope-put", OwnerKind: "personal", OwnerID: v.AccountID, Request: request}
			if mode == "pending-successor" {
				if err := v.prepareKeySuccessor(ctx, &record, &keys); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SavePasswordVault(record); err != nil {
				t.Fatal(err)
			}
			record.Clear()
			currentState, err := control.scopeState("personal", v.AccountID, "", op.Envelope)
			if err != nil {
				t.Fatal(err)
			}
			c := &scopeConflictControl{vaultScopesControl: control, mode: mode, current: currentState}
			v.Client = c
			switch mode {
			case "competing", "later", "epoch", "wrong-target":
				concurrent := candidate.Claims
				if mode == "later" {
					concurrent.Revision++
					concurrent.Previous = candidate.ID[:]
				}
				if mode == "epoch" {
					concurrent.KeyEpoch++
					concurrent.Revision = 1
					concurrent.Previous = make([]byte, 32)
				}
				if mode == "wrong-target" {
					concurrent.OwnerID = "other_account"
					concurrent.WriterAccount = "other_account"
				}
				next, err := environmente2ee.SealVaultScope(ctx, concurrent, keys.PersonalKey, keys.WriterSeed, map[string][]byte{})
				if err != nil {
					t.Fatal(err)
				}
				c.current, err = control.scopeState(concurrent.OwnerKind, concurrent.OwnerID, concurrent.MachineID, vaultEncoded(next.Raw))
				if err != nil {
					t.Fatal(err)
				}
			case "stale":
				c.readErr = &api.APIError{Status: 404}
			case "invalid-signature":
				raw, err := decodeVaultTestEnvelope(c.current.Envelope)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				c.current.Envelope = vaultEncoded(raw)
			case "forbidden":
				c.readErr = &api.APIError{Status: 403, Code: "team_entitlement_required"}
			case "ambiguous":
				c.readErr = errors.New("private transport failure")
			}
			if mode == "committed" {
				if err := store.LockPasswordVault(v.Issuer, v.AccountID); err != nil {
					t.Fatal(err)
				}
			}
			result := v.Resume(ctx)
			clears := mode == "committed" || mode == "competing" || mode == "later" || mode == "epoch"
			if mode == "committed" {
				if result != nil {
					t.Fatal(result)
				}
			} else if clears {
				var conflict *ScopeRefreshConflict
				if !errors.As(result, &conflict) || conflict.EpochChanged != (mode == "epoch") {
					t.Fatal("verified conflict lacked recovery")
				}
			} else {
				var pending *ScopePublicationPending
				if !errors.As(result, &pending) {
					t.Fatal("unproven cursor did not retain publication")
				}
			}
			after, err := store.LoadPasswordVault(v.Issuer, v.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			defer after.Clear()
			if (after.Operation == nil) != clears {
				t.Fatal("journal clear lacked exact proof")
			}
			if after.Head != before.Head || !bytes.Equal(after.Envelope, before.Envelope) || (mode != "committed" && !bytes.Equal(after.Payload, before.Payload)) {
				t.Fatal("scope conflict changed custody or floor")
			}
			if mode == "pending-successor" && after.Pending == nil {
				t.Fatal("key successor was discarded")
			}
			if !clears {
				if !bytes.Equal(after.Operation.Request, request) {
					t.Fatal("pending operation changed ciphertext")
				}
				if err := v.Resume(ctx); err == nil {
					t.Fatal("unproven retry falsely completed")
				}
				if len(c.requests) != 2 || c.requests[0] != c.requests[1] {
					t.Fatal("pending replay changed exact bytes")
				}
			}

		})
	}
}

func TestVaultScopeMissingStatusCannotHideOperationalCause(t *testing.T) {
	for _, operation := range []string{"list", "mutate"} {
		t.Run(operation, func(t *testing.T) {
			vault, control, store := newVaultScopesFixture(t)
			defer func() {
				record, err := store.LoadPasswordVault(vault.Issuer, vault.AccountID)
				if err != nil {
					t.Fatal(err)
				}
				defer record.Clear()
				if record.Operation != nil || len(control.scopePuts) != 0 {
					t.Fatal("mixed not-found and I/O failure authorized scope mutation")
				}
			}()
			failure := errors.Join(&api.APIError{Status: 404}, syscall.EIO)
			client := &scopeConflictControl{vaultScopesControl: control, mode: operation, readErr: failure}
			vault.Client = client

			var err error
			if operation == "list" {
				var names []string
				names, err = vault.ListScopeNames(context.Background(), "personal", vault.AccountID, "")
				if err == nil || len(names) != 0 {
					t.Fatal("scope outage was presented as an empty list")
				}
			} else {
				err = vault.MutateScope(context.Background(), "personal", vault.AccountID, "", func(values map[string][]byte) error {
					values["NAME"] = []byte("value")
					return nil
				})
			}
			if !errors.Is(err, syscall.EIO) {
				t.Fatalf("operational read cause was lost: %v", err)
			}
			var apiErr *api.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 404 {
				t.Fatalf("original not-found response was lost: %v", err)
			}
		})
	}
}
