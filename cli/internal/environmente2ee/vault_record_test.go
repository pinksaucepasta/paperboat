package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestVaultRecordsIndependentWritesAndScopeIsolation(t *testing.T) {
	ctx := context.Background()
	keys, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	key, err := VaultRecordKey(keys.PersonalKey, "team_1", "personal", "account_1", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	writer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	defer clear(writer)
	claims := VaultRecordClaims{Issuer: "https://control.example", WorkspaceID: "team_1", OwnerKind: "personal", OwnerID: "account_1", KeyEpoch: 1, Revision: 1, WriterAccount: "account_1", WriterVaultGeneration: 1}
	first, err := SealVaultRecord(ctx, claims, key, keys.WriterSeed, "KEY_1", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := SealVaultRecord(ctx, claims, key, keys.WriterSeed, "KEY_2", []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Claims.RecordID == second.Claims.RecordID || bytes.Contains(first.Raw, []byte("KEY_1")) || bytes.Contains(first.Raw, []byte("first")) {
		t.Fatal("record identities or ciphertext confidentiality failed")
	}
	claims.Revision = 2
	updated, err := SealVaultRecord(ctx, claims, key, keys.WriterSeed, "KEY_1", []byte("updated"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		record      VaultRecord
		name, value string
	}{{updated, "KEY_1", "updated"}, {second, "KEY_2", "second"}} {
		parsed, err := ParseVaultRecord(entry.record.Raw, writer.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		name, value, err := OpenVaultRecord(ctx, parsed, key)
		if err != nil || name != entry.name || string(value) != entry.value {
			clear(value)
			t.Fatal("independent encrypted record failed")
		}
		clear(value)
	}
	if updated.Claims.RecordID != first.Claims.RecordID {
		t.Fatal("editing changed record identity")
	}
	for _, coordinate := range []struct {
		workspace, machine string
		epoch              uint64
	}{{"personal", "", 1}, {"team_2", "", 1}, {"team_1", "machine_1", 1}, {"team_1", "", 2}} {
		other, err := VaultRecordKey(keys.PersonalKey, coordinate.workspace, "personal", "account_1", coordinate.machine, coordinate.epoch)
		if err != nil {
			t.Fatal(err)
		}
		_, value, err := OpenVaultRecord(ctx, first, other)
		clear(value)
		clear(other)
		if err == nil {
			t.Fatal("record opened with a different scope or epoch key")
		}
	}
	caseID, err := VaultRecordIdentifier(key, "key_1")
	if err != nil || caseID != first.Claims.RecordID {
		t.Fatal("case-folded duplicate identity not preserved")
	}
	corrupt := bytes.Clone(first.Raw)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := ParseVaultRecord(corrupt, writer.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("tampered signature accepted")
	}
	claims.Deleted = true
	deleted, err := SealVaultRecord(ctx, claims, key, keys.WriterSeed, "KEY_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	name, value, err := OpenVaultRecord(ctx, deleted, key)
	defer clear(value)
	if err != nil || name != "KEY_1" || len(value) != 0 || deleted.Claims.RecordID != first.Claims.RecordID {
		t.Fatal("encrypted tombstone lost identity")
	}
	if _, err := SealVaultRecord(ctx, claims, key, keys.WriterSeed, "KEY_1", []byte("must not survive deletion")); err == nil {
		t.Fatal("nonempty tombstone accepted")
	}
	claims.Deleted = false
	for _, value := range [][]byte{{0}, {0xff}, make([]byte, MaximumValueBytes+1)} {
		if _, err := SealVaultRecord(ctx, claims, key, keys.WriterSeed, "KEY_1", value); err == nil {
			t.Fatal("invalid process value accepted")
		}
	}
}

func TestVaultScopeKeyGrantDecryptsIndependentRecords(t *testing.T) {
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
	key, err := VaultRecordKey(keys.PersonalKey, "personal", "personal", "account_1", "machine_1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	anchor, err := SealVaultScope(ctx, VaultScopeClaims{Issuer: "https://control.example", WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1", MachineID: "machine_1", KeyEpoch: 1, Revision: 1, Previous: make([]byte, 32), WriterAccount: "account_1", WriterVaultGeneration: 1}, keys.PersonalKey, keys.WriterSeed, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	claims := VaultLayerClaims{Issuer: "https://control.example", RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: host.PublicKey().Bytes(), DeliveryGeneration: 1, Previous: make([]byte, 32), FenceGeneration: 1, Source: VaultLayerSource{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1", MachineID: "machine_1", KeyEpoch: 1, Revision: 1, Digest: anchor.ID[:]}, WriterAccount: "account_1", WriterVaultGeneration: 1}
	grant, err := SealVaultScopeKey(ctx, claims, keys.WriterSeed, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseVaultLayer(grant.Raw, grant.Claims.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	deviceKey, err := OpenVaultScopeKey(ctx, parsed, parsed.Claims, host.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(deviceKey)
	for _, revision := range []uint64{1, 2, 3} {
		record, err := SealVaultRecord(ctx, VaultRecordClaims{Issuer: claims.Issuer, WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1", MachineID: "machine_1", KeyEpoch: 1, Revision: revision, WriterAccount: "account_1", WriterVaultGeneration: 1}, key, keys.WriterSeed, "KEY_1", []byte("value"))
		if err != nil {
			t.Fatal(err)
		}
		_, value, err := OpenVaultRecord(ctx, record, deviceKey)
		clear(value)
		if err != nil {
			t.Fatal("unchanged key grant could not decrypt an ordinary edit")
		}
	}
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := OpenVaultScopeKey(ctx, parsed, parsed.Claims, other.Bytes()); err == nil {
		clear(k)
		t.Fatal("grant opened by another device")
	}
	changed := parsed.Claims
	changed.InstallationGeneration++
	if k, err := OpenVaultScopeKey(ctx, parsed, changed, host.Bytes()); err == nil {
		clear(k)
		t.Fatal("stale installation grant accepted")
	}
}
