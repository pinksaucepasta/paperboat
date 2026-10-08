package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"testing"
)

func TestVaultScopeAndAddressedTeamGrant(t *testing.T) {
	ctx := context.Background()
	owner, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Clear()
	member, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer member.Clear()
	writer := ed25519.NewKeyFromSeed(owner.WriterSeed)
	defer clear(writer)
	writerPublic := writer.Public().(ed25519.PublicKey)
	scope, err := SealVaultScope(ctx, VaultScopeClaims{Issuer: "https://control.example", OwnerKind: "team", OwnerID: "team_1", WorkspaceID: "team_1", KeyEpoch: 1, Revision: 1, Previous: make([]byte, 32), WriterAccount: "owner_1", WriterVaultGeneration: 1}, owner.PersonalKey, owner.WriterSeed, map[string][]byte{"SELECTED": []byte("selected-test-value"), "UNSELECTED": []byte("unselected-test-value")})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseVaultScope(scope.Raw, writerPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := OpenVaultScope(ctx, parsed, owner.PersonalKey)
	if err != nil || len(values) != 2 {
		t.Fatal("scope round trip failed", err)
	}
	defer clearValues(values)
	tampered := bytes.Clone(scope.Raw)
	tampered[len(tampered)-1] ^= 1
	if _, err := ParseVaultScope(tampered, writerPublic); err == nil {
		t.Fatal("tampered scope accepted")
	}
	if _, err := OpenVaultScope(ctx, parsed, member.PersonalKey); err == nil {
		t.Fatal("wrong scope key accepted")
	}
	memberSharing, _ := ecdh.X25519().NewPrivateKey(member.SharingPrivate)
	grant, err := SealTeamGrant(ctx, TeamGrantClaims{Issuer: "https://control.example", TeamID: "team_1", TeamEpoch: 1, MembershipGeneration: 2, SenderAccount: "owner_1", RecipientAccount: "member_1", RecipientVaultGeneration: 3, RecipientSharingPublic: memberSharing.PublicKey().Bytes(), OperationID: "op_1"}, owner.PersonalKey, owner.WriterSeed)
	if err != nil {
		t.Fatal(err)
	}
	parsedGrant, err := ParseTeamGrant(grant.Raw, writerPublic)
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := OpenTeamGrant(ctx, parsedGrant, "https://control.example", "member_1", 4, 1, 2, member.SharingPrivate)
	if err != nil || !bytes.Equal(delivered, owner.PersonalKey) {
		t.Fatal("grant did not deliver exact key", err)
	}
	clear(delivered)
	for _, test := range []struct {
		account           string
		epoch, membership uint64
		private           []byte
	}{{"other_1", 1, 2, member.SharingPrivate}, {"member_1", 2, 2, member.SharingPrivate}, {"member_1", 1, 3, member.SharingPrivate}, {"member_1", 1, 2, owner.SharingPrivate}} {
		if key, err := OpenTeamGrant(ctx, parsedGrant, "https://control.example", test.account, 4, test.epoch, test.membership, test.private); err == nil {
			clear(key)
			t.Fatal("unauthorized grant consumption accepted")
		}
	}
	for _, raw := range [][]byte{scope.Raw, grant.Raw} {
		if bytes.Contains(raw, owner.PersonalKey) || bytes.Contains(raw, owner.WriterSeed) || bytes.Contains(raw, []byte("selected-test-value")) || bytes.Contains(raw, []byte("unselected-test-value")) {
			t.Fatal("document leaked secret bytes")
		}
	}
}
