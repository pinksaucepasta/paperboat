package environmente2ee

import (
	"bytes"
	"testing"
)

func TestVaultKeysRoundTripAndInventoryValidation(t *testing.T) {
	keys, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	other, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Clear()
	if bytes.Equal(keys.PersonalKey, other.PersonalKey) || bytes.Equal(keys.SharingPrivate, other.SharingPrivate) || bytes.Equal(keys.PersonalKey, keys.SharingPrivate) {
		t.Fatal("keys were reused")
	}
	if bytes.Equal(keys.WriterSeed, other.WriterSeed) || len(keys.WriterSeed) != 32 {
		t.Fatal("writer seeds were reused or omitted")
	}
	keys.Teams = []VaultTeamKey{{TeamID: "team_1", Epoch: 1, MembershipGeneration: 2, Key: bytes.Repeat([]byte{1}, 32)}}
	raw, err := keys.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	decoded, err := ParseVaultKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Clear()
	if !bytes.Equal(decoded.PersonalKey, keys.PersonalKey) || !bytes.Equal(decoded.SharingPrivate, keys.SharingPrivate) || !bytes.Equal(decoded.WriterSeed, keys.WriterSeed) || !bytes.Equal(decoded.Teams[0].Key, keys.Teams[0].Key) {
		t.Fatal("key inventory changed")
	}
	keys.Teams = append(keys.Teams, keys.Teams[0])
	if _, err := keys.MarshalBinary(); err == nil {
		t.Fatal("duplicate team accepted")
	}
	keys.Teams = nil
	if _, err := keys.MarshalBinary(); err == nil {
		t.Fatal("null inventory accepted")
	}
	keys.Teams = []VaultTeamKey{}
	keys.PersonalEpoch = 0
	if _, err := keys.MarshalBinary(); err == nil {
		t.Fatal("zero epoch accepted")
	}
}

func TestVaultKeysRejectsMalformedPrivateMaterial(t *testing.T) {
	keys, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	cases := []struct {
		name string
		edit func(*VaultKeys)
	}{
		{name: "writer-seed-length", edit: func(k *VaultKeys) { k.WriterSeed = []byte{1} }},
		{name: "writer-seed-zero", edit: func(k *VaultKeys) { k.WriterSeed = bytes.Repeat([]byte{0}, 32) }},
		{name: "sharing-private-length", edit: func(k *VaultKeys) { k.SharingPrivate = []byte{1} }},
		{name: "sharing-private-zero", edit: func(k *VaultKeys) { k.SharingPrivate = bytes.Repeat([]byte{0}, 32) }},
		{name: "personal-key-zero", edit: func(k *VaultKeys) { k.PersonalKey = bytes.Repeat([]byte{0}, 32) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := keys
			candidate.WriterSeed = bytes.Clone(keys.WriterSeed)
			candidate.SharingPrivate = bytes.Clone(keys.SharingPrivate)
			candidate.PersonalKey = bytes.Clone(keys.PersonalKey)
			candidate.Teams = make([]VaultTeamKey, len(keys.Teams))
			for i := range keys.Teams {
				candidate.Teams[i] = keys.Teams[i]
				candidate.Teams[i].Key = bytes.Clone(keys.Teams[i].Key)
			}
			tc.edit(&candidate)
			if _, err := candidate.MarshalBinary(); err == nil {
				t.Fatal("malformed private material accepted")
			}
			candidate.Clear()
		})
	}
}

func TestVaultKeysMarshalPreservesCallerBuffers(t *testing.T) {
	keys, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	keys.Teams = []VaultTeamKey{{TeamID: "team_1", Epoch: 1, MembershipGeneration: 1, Key: bytes.Repeat([]byte{7}, 32)}}
	beforeWriter := bytes.Clone(keys.WriterSeed)
	beforePersonal := bytes.Clone(keys.PersonalKey)
	beforeSharing := bytes.Clone(keys.SharingPrivate)
	beforeTeam := bytes.Clone(keys.Teams[0].Key)
	raw, err := keys.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	if !bytes.Equal(keys.WriterSeed, beforeWriter) || !bytes.Equal(keys.PersonalKey, beforePersonal) || !bytes.Equal(keys.SharingPrivate, beforeSharing) || !bytes.Equal(keys.Teams[0].Key, beforeTeam) {
		t.Fatal("marshal mutated caller key buffers")
	}
}
