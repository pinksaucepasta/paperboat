package environmente2ee

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestVaultScopeGrantAndLeastPrivilegeProjection(t *testing.T) {
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
	scope, err := SealVaultScope(ctx, VaultScopeClaims{Issuer: "https://control.example", OwnerKind: "team", OwnerID: "team_1", KeyEpoch: 1, Revision: 1, Previous: make([]byte, 32), WriterAccount: "owner_1", WriterVaultGeneration: 1}, owner.PersonalKey, owner.WriterSeed, map[string][]byte{"SELECTED": []byte("selected-test-value"), "UNSELECTED": []byte("unselected-test-value")})
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
	host, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := SealHostProjection(ctx, HostProjectionClaims{Issuer: "https://control.example", OwnerAccount: "owner_1", MachineID: "machine_1", InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: host.PublicKey().Bytes(), SelectionGeneration: 1, Revision: 1, Previous: make([]byte, 32), Sources: []ProjectionSource{{OwnerKind: "team", OwnerID: "team_1", KeyEpoch: 1, Revision: 1, Digest: scope.ID[:]}}, WriterVaultGeneration: 1}, owner.WriterSeed, map[string][]byte{"SELECTED": values["SELECTED"]})
	if err != nil {
		t.Fatal(err)
	}
	parsedProjection, err := ParseHostProjection(projection.Raw, writerPublic)
	if err != nil {
		t.Fatal(err)
	}
	received, err := OpenHostProjection(ctx, parsedProjection, projection.Claims, host.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer clearValues(received)
	if len(received) != 1 || !bytes.Equal(received["SELECTED"], values["SELECTED"]) {
		t.Fatal("projection delivered unintended values")
	}
	if _, ok := received["UNSELECTED"]; ok {
		t.Fatal("host received unselected value")
	}
	if key, err := OpenTeamGrant(ctx, parsedGrant, "https://control.example", "member_1", 4, 1, 2, host.Bytes()); err == nil {
		clear(key)
		t.Fatal("host obtained scope key")
	}
	wrong := projection.Claims
	wrong.InstallationGeneration++
	if _, err := OpenHostProjection(ctx, parsedProjection, wrong, host.Bytes()); err == nil {
		t.Fatal("reinstalled host accepted old projection")
	}
	wrong = projection.Claims
	wrong.SelectionGeneration++
	if _, err := OpenHostProjection(ctx, parsedProjection, wrong, host.Bytes()); err == nil {
		t.Fatal("stale selection accepted")
	}
	for _, raw := range [][]byte{scope.Raw, grant.Raw, projection.Raw} {
		if bytes.Contains(raw, owner.PersonalKey) || bytes.Contains(raw, owner.WriterSeed) || bytes.Contains(raw, []byte("selected-test-value")) || bytes.Contains(raw, []byte("unselected-test-value")) {
			t.Fatal("document leaked secret bytes")
		}
	}
}
