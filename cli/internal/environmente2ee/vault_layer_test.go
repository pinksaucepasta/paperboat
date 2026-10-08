package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"testing"
)

func TestVaultLayerExactBindingAndWorkspaceIsolation(t *testing.T) {
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
	source, err := SealVaultScope(ctx, VaultScopeClaims{Issuer: "https://control.example", OwnerKind: "personal", OwnerID: "account_1", WorkspaceID: "team_1", KeyEpoch: 1, Revision: 1, Previous: make([]byte, 32), WriterAccount: "account_1", WriterVaultGeneration: 1}, keys.PersonalKey, keys.WriterSeed, map[string][]byte{"VALUE": []byte("member")})
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
	personalKey, _ := WorkspaceScopeKey(keys.PersonalKey, "personal")
	defer clear(personalKey)
	teamKey, _ := WorkspaceScopeKey(keys.PersonalKey, "team_1")
	defer clear(teamKey)
	otherKey, _ := WorkspaceScopeKey(keys.PersonalKey, "team_2")
	defer clear(otherKey)
	if bytes.Equal(teamKey, personalKey) || bytes.Equal(teamKey, otherKey) {
		t.Fatal("workspace key domains collide")
	}
	claims := VaultLayerClaims{Issuer: "https://control.example", RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: host.PublicKey().Bytes(), DeliveryGeneration: 1, Previous: make([]byte, 32), FenceGeneration: 1, Source: VaultLayerSource{WorkspaceID: "team_1", OwnerKind: "personal", OwnerID: "account_1", KeyEpoch: 1, Revision: 1, Digest: source.ID[:]}, WriterAccount: "account_1", WriterVaultGeneration: 1}
	layer, err := SealVaultLayer(ctx, claims, keys.WriterSeed, map[string][]byte{"VALUE": []byte("member")})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseVaultLayer(layer.Raw, layer.Claims.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := OpenVaultLayer(ctx, parsed, layer.Claims, host.Bytes())
	if err != nil || string(values["VALUE"]) != "member" {
		t.Fatal("exact layer failed")
	}
	for _, v := range values {
		clear(v)
	}
	for _, mutate := range []func(*VaultLayerClaims){func(c *VaultLayerClaims) { c.Source.WorkspaceID = "personal" }, func(c *VaultLayerClaims) { c.Source.Revision++ }, func(c *VaultLayerClaims) { c.FenceGeneration++ }, func(c *VaultLayerClaims) { c.MachineID = "machine_2" }, func(c *VaultLayerClaims) { c.RecipientAccount = "account_2" }, func(c *VaultLayerClaims) { c.InstallationGeneration++ }} {
		expected := layer.Claims
		mutate(&expected)
		if values, err := OpenVaultLayer(ctx, parsed, expected, host.Bytes()); err == nil {
			for _, v := range values {
				clear(v)
			}
			t.Fatal("changed layer binding accepted")
		}
	}
	claims.RecipientAccount = "account_2"
	if _, err := SealVaultLayer(ctx, claims, keys.WriterSeed, map[string][]byte{}); err == nil {
		t.Fatal("private member layer sent to foreign device")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := OpenVaultLayer(cancelled, parsed, parsed.Claims, host.Bytes()); err == nil {
		t.Fatal("cancelled open accepted")
	}
}
