package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"testing"

	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

func hostRecipient(t *testing.T, account, machine string) (layerRecipient, *ecdh.PrivateKey) {
	t.Helper()
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return layerRecipient{RecipientAccount: account, MachineID: machine, InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: encoded(private.PublicKey().Bytes()), FenceGeneration: 1}, private
}

func layerRequest(v vaultState, anchor scopeState, r layerRecipient) request {
	return request{Action: "host-layer", Vault: v, Scope: &anchor, WorkspaceID: anchor.WorkspaceID, LayerRecipient: &r, Operation: "operation_test"}
}

func TestHostLayerSealsOnlyToBoundRecipientAndOpensNatively(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	anchor := createAnchor(t, e, v, "personal", "personal", v.Account, "")
	recipient, private := hostRecipient(t, v.Account, "machine-one")
	out, err := e.run(ctx, layerRequest(v, anchor, recipient))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decode(out.(map[string]string)["envelope"])
	if err != nil {
		t.Fatal(err)
	}
	protection, err := env.PasswordVaultProtection(e.raw)
	if err != nil {
		t.Fatal(err)
	}
	layer, err := env.ParseVaultLayer(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	key, err := env.OpenVaultScopeKey(ctx, layer, layer.Claims, private.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The host opens the same record key the writer derives for variable records.
	want, err := env.VaultRecordKey(e.keys.PersonalKey, "personal", "personal", v.Account, "", e.keys.PersonalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, want) {
		t.Fatal("host layer did not carry the record key")
	}
}

func TestHostLayerRefusesForgedOrUnboundRecipients(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	anchor := createAnchor(t, e, v, "personal", "personal", v.Account, "")
	good, _ := hostRecipient(t, v.Account, "machine-one")
	override := createAnchor(t, e, v, "personal", "personal", v.Account, "machine-one")
	lowOrder := good
	lowOrder.HostPublic = encoded(make([]byte, 32))
	shortKey := good
	shortKey.HostPublic = encoded([]byte{1, 2, 3})
	notBase64 := good
	notBase64.HostPublic = "!!"
	otherAccount := good
	otherAccount.RecipientAccount = "someone-else"
	noGeneration := good
	noGeneration.HostKeyGeneration = 0
	brokenChain := good
	brokenChain.DeliveryGeneration = 1 // a prior delivery needs its document id
	for name, c := range map[string]struct {
		anchor    scopeState
		recipient layerRecipient
	}{
		"all-zero host key":           {anchor, lowOrder},
		"short host key":              {anchor, shortKey},
		"malformed host key":          {anchor, notBase64},
		"another account":             {anchor, otherAccount},
		"no host key generation":      {anchor, noGeneration},
		"delivery chain without id":   {anchor, brokenChain},
		"override for another device": {override, func() layerRecipient { r, _ := hostRecipient(t, v.Account, "machine-two"); return r }()},
	} {
		if _, err := e.run(ctx, layerRequest(v, c.anchor, c.recipient)); err == nil {
			t.Fatalf("%s: forged recipient was sealed to", name)
		}
	}
	if _, err := e.run(ctx, layerRequest(v, override, good)); err != nil {
		t.Fatalf("matching device override refused: %v", err)
	}
}

func TestTeamLayerRecipientMustBeActiveReader(t *testing.T) {
	team := &teamState{ID: "team-one", Members: []teamMember{
		{Account: "reader", Active: true, Permission: "read"},
		{Account: "gone", Active: false, Permission: "write"},
		{Account: "none", Active: true, Permission: ""},
	}}
	claims := env.VaultScopeClaims{OwnerKind: "team", OwnerID: "team-one", WorkspaceID: "team-one"}
	for account, ok := range map[string]bool{"reader": true, "gone": false, "none": false, "stranger": false} {
		err := authorizeLayerRecipient("writer", claims, team, layerRecipient{RecipientAccount: account, MachineID: "m", InstallationGeneration: 1, HostKeyGeneration: 1})
		if (err == nil) != ok {
			t.Fatalf("recipient %q: err=%v want ok=%v", account, err, ok)
		}
	}
	if authorizeLayerRecipient("writer", claims, nil, layerRecipient{RecipientAccount: "reader", MachineID: "m", InstallationGeneration: 1, HostKeyGeneration: 1}) == nil {
		t.Fatal("team layer without team state accepted")
	}
}
