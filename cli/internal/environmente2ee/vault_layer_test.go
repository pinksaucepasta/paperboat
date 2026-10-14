package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"testing"
)

func TestVaultLayerExactScopeKeyGrantBindingAndWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	keys, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	host, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongHost, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	source, err := SealVaultScope(ctx, VaultScopeClaims{
		Issuer:                "https://control.example",
		OwnerKind:             "personal",
		OwnerID:               "account_1",
		WorkspaceID:           "team_1",
		KeyEpoch:              1,
		Revision:              1,
		Previous:              make([]byte, 32),
		WriterAccount:         "account_1",
		WriterVaultGeneration: 1,
	}, keys.PersonalKey, keys.WriterSeed, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	changed := source
	changed.Claims.WorkspaceID = "personal"
	if values, err := OpenVaultScope(ctx, changed, keys.PersonalKey); err == nil {
		for _, v := range values {
			clear(v)
		}
		t.Fatal("cross-workspace scope opened")
	}
	scopeKey, err := VaultRecordKey(keys.PersonalKey, "team_1", "personal", "account_1", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(scopeKey)
	personalKey, err := VaultRecordKey(keys.PersonalKey, "personal", "personal", "account_1", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(personalKey)
	otherTeamKey, err := VaultRecordKey(keys.PersonalKey, "team_2", "personal", "account_1", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(otherTeamKey)
	if bytes.Equal(scopeKey, personalKey) || bytes.Equal(scopeKey, otherTeamKey) {
		t.Fatal("workspace key domains collide")
	}
	claims := VaultLayerClaims{
		Issuer:                 "https://control.example",
		RecipientAccount:       "account_1",
		MachineID:              "machine_1",
		InstallationGeneration: 1,
		HostKeyGeneration:      1,
		HostPublic:             host.PublicKey().Bytes(),
		DeliveryGeneration:     1,
		Previous:               make([]byte, 32),
		FenceGeneration:        1,
		Source: VaultLayerSource{
			WorkspaceID: "team_1",
			OwnerKind:   "personal",
			OwnerID:     "account_1",
			KeyEpoch:    1,
			Revision:    1,
			Digest:      source.ID[:],
		},
		WriterAccount:         "account_1",
		WriterVaultGeneration: 1,
	}
	grant, err := SealVaultScopeKey(ctx, claims, keys.WriterSeed, scopeKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseVaultLayer(grant.Raw, grant.Claims.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenVaultScopeKey(ctx, parsed, parsed.Claims, host.Bytes())
	if err != nil || !bytes.Equal(opened, scopeKey) {
		clear(opened)
		t.Fatal("exact scope-key grant failed")
	}
	clear(opened)
	for _, mutate := range []func(*VaultLayerClaims){
		func(c *VaultLayerClaims) { c.Source.WorkspaceID = "personal" },
		func(c *VaultLayerClaims) { c.Source.Revision++ },
		func(c *VaultLayerClaims) { c.FenceGeneration++ },
		func(c *VaultLayerClaims) { c.MachineID = "machine_2" },
		func(c *VaultLayerClaims) { c.RecipientAccount = "account_2" },
		func(c *VaultLayerClaims) { c.InstallationGeneration++ },
		func(c *VaultLayerClaims) { c.HostKeyGeneration++ },
	} {
		expected := grant.Claims
		mutate(&expected)
		if key, err := OpenVaultScopeKey(ctx, parsed, expected, host.Bytes()); err == nil {
			clear(key)
			t.Fatal("changed grant binding accepted")
		}
	}
	if _, err := OpenVaultScopeKey(ctx, parsed, parsed.Claims, wrongHost.Bytes()); err == nil {
		t.Fatal("grant opened for a different host key")
	}
	claims.RecipientAccount = "account_2"
	if _, err := SealVaultScopeKey(ctx, claims, keys.WriterSeed, scopeKey); err == nil {
		t.Fatal("private member key sent to a foreign device")
	}
	tampered := parsed
	tampered.Raw = bytes.Clone(parsed.Raw)
	tampered.Raw[len(tampered.Raw)-1] ^= 1
	tampered.ID = DocumentID(sha256.Sum256(tampered.Raw))
	if _, err := ParseVaultLayer(tampered.Raw, parsed.Claims.WriterPublic); err == nil {
		t.Fatal("tampered scope-key grant signature accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := OpenVaultScopeKey(cancelled, parsed, parsed.Claims, host.Bytes()); err == nil {
		t.Fatal("cancelled grant open accepted")
	}
}
