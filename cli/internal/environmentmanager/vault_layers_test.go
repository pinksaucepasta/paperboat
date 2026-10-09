package environmentmanager

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type layerControl struct {
	*vaultScopesControl
	recipients []api.VaultLayerRecipient
	deliveries map[string]api.VaultLayerDelivery
	operations map[string]api.VaultLayerDelivery
	requests   []api.VaultLayerPut
	fail       string
}

func (c *layerControl) VaultLayerRecipients(context.Context, api.VaultLayerCoordinate) ([]api.VaultLayerRecipient, error) {
	return append([]api.VaultLayerRecipient(nil), c.recipients...), nil
}
func (c *layerControl) VaultAllLayerRecipients(context.Context, api.VaultLayerCoordinate) ([]api.VaultLayerRecipient, error) {
	return append([]api.VaultLayerRecipient(nil), c.recipients...), nil
}
func (c *layerControl) PutVaultLayer(ctx context.Context, workspace, machine string, in api.VaultLayerPut) (api.VaultLayerDelivery, error) {
	c.requests = append(c.requests, in)
	if c.fail == "forbidden" {
		return api.VaultLayerDelivery{}, &api.APIError{Status: 403, Code: "permission_denied"}
	}
	if c.fail == "rotation" {
		return api.VaultLayerDelivery{}, &api.APIError{Status: 409, Code: "rotation_required"}
	}
	if c.fail == "ambiguous" {
		return api.VaultLayerDelivery{}, errors.New("private network failure")
	}
	if c.fail == "conflict" {
		return api.VaultLayerDelivery{}, &api.APIError{Status: 409, Code: "version_conflict"}
	}
	if previous, ok := c.operations[in.OperationID]; ok {
		if previous.Envelope != in.Envelope {
			return api.VaultLayerDelivery{}, errors.New("changed retry")
		}
		return previous, nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(in.Envelope)
	if err != nil {
		return api.VaultLayerDelivery{}, err
	}
	layer, err := env.ParseVaultLayer(raw, c.writerPublic)
	if err != nil {
		return api.VaultLayerDelivery{}, err
	}
	claims := layer.Claims
	if claims.Source.WorkspaceID != workspace || claims.MachineID != machine {
		return api.VaultLayerDelivery{}, ErrIntegrity
	}
	var recipient api.VaultLayerRecipient
	for i, r := range c.recipients {
		if r.MachineID == machine {
			recipient = r
			recipient.DeliveryGeneration = claims.DeliveryGeneration
			recipient.DocumentID = layer.ID.String()
			c.recipients[i] = recipient
		}
	}
	out := api.VaultLayerDelivery{Recipient: recipient, Source: api.VaultLayerSource{VaultLayerCoordinate: api.VaultLayerCoordinate{WorkspaceID: workspace, OwnerKind: claims.Source.OwnerKind, OwnerID: claims.Source.OwnerID, MachineID: claims.Source.MachineID}, KeyEpoch: claims.Source.KeyEpoch, Revision: claims.Source.Revision, DocumentID: env.DocumentID(*(*[32]byte)(claims.Source.Digest)).String()}, Envelope: in.Envelope, WriterAccount: claims.WriterAccount, WriterPublic: base64.RawURLEncoding.EncodeToString(claims.WriterPublic), State: "ready"}
	c.deliveries[machine] = out
	c.operations[in.OperationID] = out
	if c.fail == "lost" {
		c.fail = ""
		return api.VaultLayerDelivery{}, errors.New("lost publication response")
	}
	return out, nil
}
func TestVaultLayerFanoutAutomaticGlobalAndExactResume(t *testing.T) {
	v, control, store := newVaultScopesFixture(t)
	ctx := context.Background()
	if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(values map[string][]byte) error {
		values["FIRST"] = []byte("one")
		values["SECOND"] = []byte("two")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	host, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &layerControl{vaultScopesControl: control, deliveries: map[string]api.VaultLayerDelivery{}, operations: map[string]api.VaultLayerDelivery{}}
	for _, machine := range []string{"machine_1", "machine_2"} {
		c.recipients = append(c.recipients, api.VaultLayerRecipient{RecipientAccount: v.AccountID, MachineID: machine, InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: base64.RawURLEncoding.EncodeToString(host.PublicKey().Bytes()), FenceGeneration: 1})
	}
	v.Client = c
	if n, err := v.RefreshSource(ctx, "personal", v.AccountID, ""); err != nil || n != 2 {
		t.Fatal("global did not reach both authorized devices")
	}
	for _, delivery := range c.deliveries {
		raw, _ := base64.RawURLEncoding.Strict().DecodeString(delivery.Envelope)
		layer, err := env.ParseVaultLayer(raw, control.writerPublic)
		if err != nil {
			t.Fatal(err)
		}
		values, err := openLayerRecordValues(ctx, c, delivery, layer, host.Bytes())
		if err != nil || string(values["FIRST"]) != "one" || string(values["SECOND"]) != "two" {
			t.Fatal("global values required a name selection")
		}
		clearVaultValues(values)
	}
	if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(values map[string][]byte) error { clear(values["FIRST"]); delete(values, "FIRST"); return nil }); err != nil {
		t.Fatal(err)
	}
	c.fail = "lost"
	if _, err := v.RefreshSource(ctx, "personal", v.AccountID, ""); err == nil {
		t.Fatal("lost response concealed")
	}
	record, err := store.LoadPasswordVault(v.Issuer, v.AccountID)
	if err != nil || record.Operation == nil || record.Operation.Kind != "layer-put" {
		t.Fatal("exact layer retry not durable")
	}
	record.Clear()
	first := c.requests[len(c.requests)-1]
	if err := v.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	last := c.requests[len(c.requests)-1]
	if first != last {
		t.Fatal("resume changed encrypted layer operation")
	}
	delivery := c.deliveries["machine_1"]
	raw, _ := base64.RawURLEncoding.Strict().DecodeString(delivery.Envelope)
	layer, err := env.ParseVaultLayer(raw, control.writerPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := openLayerRecordValues(ctx, c, delivery, layer, host.Bytes())
	if err != nil || len(values) != 1 || string(values["SECOND"]) != "two" {
		t.Fatal("removed global value survived delivery")
	}
	clearVaultValues(values)
}

func TestVaultLayerRejectedDeliveryReleasesOnlyProvenLayerJournal(t *testing.T) {
	for _, mode := range []string{"committed", "superseded", "retired", "replacement", "forbidden", "rotation", "ambiguous", "invalid-source"} {
		t.Run(mode, func(t *testing.T) {
			v, control, store := newVaultScopesFixture(t)
			ctx := context.Background()
			if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(values map[string][]byte) error { values["VALUE"] = []byte("saved"); return nil }); err != nil {
				t.Fatal(err)
			}
			host, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			c := &layerControl{vaultScopesControl: control, deliveries: map[string]api.VaultLayerDelivery{}, operations: map[string]api.VaultLayerDelivery{}, fail: "lost", recipients: []api.VaultLayerRecipient{{RecipientAccount: v.AccountID, MachineID: "machine_1", InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: base64.RawURLEncoding.EncodeToString(host.PublicKey().Bytes()), FenceGeneration: 1}}}
			v.Client = c
			if _, err := v.RefreshSource(ctx, "personal", v.AccountID, ""); err == nil {
				t.Fatal("lost ACK not retained")
			}
			before, err := store.LoadPasswordVault(v.Issuer, v.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			defer before.Clear()
			c.fail = "conflict"
			switch mode {
			case "superseded":
				c.recipients[0].DeliveryGeneration++
				c.recipients[0].DocumentID = env.DocumentID([32]byte{9}).String()
			case "retired":
				c.recipients = nil
			case "replacement":
				c.recipients[0].InstallationGeneration++
			case "forbidden":
				c.fail = "forbidden"
			case "rotation":
				c.fail = "rotation"
			case "ambiguous":
				c.fail = "ambiguous"
			case "invalid-source":
				state := control.scopes[scopeControlKey("personal", v.AccountID, "")]
				raw, _ := base64.RawURLEncoding.Strict().DecodeString(state.Envelope)
				raw[len(raw)-1] ^= 1
				state.Envelope = base64.RawURLEncoding.EncodeToString(raw)
				control.scopes[scopeControlKey("personal", v.AccountID, "")] = state
			}
			err = v.Resume(ctx)
			after, loadErr := store.LoadPasswordVault(v.Issuer, v.AccountID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			defer after.Clear()
			clearExpected := mode != "ambiguous" && mode != "invalid-source"
			if (after.Operation == nil) != clearExpected {
				t.Fatal("delivery journal reconciliation violated proof boundary")
			}
			if after.Head != before.Head || after.Pending != nil || string(after.Payload) != string(before.Payload) {
				t.Fatal("delivery reconciliation modified key custody")
			}
			if mode == "committed" {
				if err != nil {
					t.Fatal(err)
				}
			} else if clearExpected {
				var changed *LayerRefreshConflict
				if !errors.As(err, &changed) || mode == "rotation" && !changed.RotationRequired {
					t.Fatal("rejected delivery incorrectly claimed success")
				}
			} else {
				var pending *LayerPublicationPending
				if !errors.As(err, &pending) {
					t.Fatal("uncertain delivery not retained")
				}
			}
		})
	}
}

func (c *layerControl) GetVaultPersonalScopes(context.Context) (api.VaultPersonalInventory, error) {
	return api.VaultPersonalInventory{}, nil
}

func TestVaultLayerReconcileAcceptsAndReusesAcknowledgedTeamGrant(t *testing.T) {
	v, control, store := newVaultScopesFixture(t)
	ctx := context.Background()
	teamKey := configureIncomingGrant(t, control, store, true, 1)
	defer clear(teamKey)
	if err := v.SyncTeamGrants(ctx); err != nil {
		t.Fatal(err)
	}
	if !control.ackCommitted {
		t.Fatal("grant not durably acknowledged")
	}
	v.WorkspaceID = "team_sync"
	if err := v.MutateScope(ctx, "team", "team_sync", "", func(values map[string][]byte) error {
		values["TEAM_GLOBAL"] = []byte("test-global")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	v.WorkspaceID = "personal"
	host, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &layerControl{vaultScopesControl: control, deliveries: map[string]api.VaultLayerDelivery{}, operations: map[string]api.VaultLayerDelivery{}, recipients: []api.VaultLayerRecipient{{RecipientAccount: v.AccountID, MachineID: "machine_team", InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: base64.RawURLEncoding.EncodeToString(host.PublicKey().Bytes()), FenceGeneration: 1}}}
	v.Client = c
	puts, acks := control.putVaultCount, control.ackCount
	if n, err := v.ReconcileLayers(ctx); err != nil || n != 1 {
		t.Fatalf("acknowledged Team global fanout count=%d error=%v", n, err)
	}
	if control.putVaultCount != puts || control.ackCount != acks {
		t.Fatal("known acknowledged Team key repeated custody writes or ACK")
	}
	delivery := c.deliveries["machine_team"]
	if delivery.Source.WorkspaceID != "team_sync" || delivery.Source.OwnerKind != "team" {
		t.Fatal("Team global was not discovered from Personal workspace")
	}
	raw, _ := base64.RawURLEncoding.Strict().DecodeString(delivery.Envelope)
	layer, err := env.ParseVaultLayer(raw, control.writerPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := openLayerRecordValues(ctx, c, delivery, layer, host.Bytes())
	defer clearVaultValues(values)
	if err != nil || string(values["TEAM_GLOBAL"]) != "test-global" {
		t.Fatal("acknowledged Team global encrypted delivery did not decrypt")
	}
}

func openLayerRecordValues(ctx context.Context, c *layerControl, delivery api.VaultLayerDelivery, layer env.VaultLayer, private []byte) (map[string][]byte, error) {
	key, err := env.OpenVaultScopeKey(ctx, layer, layer.Claims, private)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	result, err := c.VaultRecords(ctx, delivery.Source.VaultLayerCoordinate, 0)
	if err != nil {
		return nil, err
	}
	values := map[string][]byte{}
	for _, state := range result.Records {
		record, err := state.Decode()
		if err != nil {
			clearVaultValues(values)
			return nil, err
		}
		name, value, err := env.OpenVaultRecord(ctx, record, key)
		if err != nil {
			clearVaultValues(values)
			return nil, err
		}
		if state.Deleted {
			clear(value)
			continue
		}
		values[name] = value
	}
	return values, nil
}
